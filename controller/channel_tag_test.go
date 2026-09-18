/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelTagControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// EditTagChannels records a manage-audit entry on success, which reads the
	// acting user through the Redis-backed username cache before falling back
	// to the DB. Redis isn't wired up in this test binary, so leaving the
	// package default enabled would dereference a nil Redis client.
	previousRedisEnabled := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		common.RedisEnabled = previousRedisEnabled
	})

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}))
	return db
}

func createTaggedTestChannel(t *testing.T, db *gorm.DB, tag, name, models string) {
	t.Helper()
	channel := model.Channel{
		Name:   name,
		Key:    "key-" + name,
		Status: common.ChannelStatusEnabled,
		Models: models,
		Group:  "default",
		Tag:    &tag,
	}
	require.NoError(t, db.Create(&channel).Error)
}

func performGetTagModels(tag string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/channel/tag/models?tag="+tag, nil)
	GetTagModels(ctx)
	return recorder
}

type tagModelsTestResponse struct {
	Success      bool   `json:"success"`
	Data         string `json:"data"`
	ChannelCount int    `json:"channel_count"`
}

// TestGetTagModelsReturnsUnionAcrossChannels guards against reintroducing the
// old "longest channel wins" behavior: channels sharing a tag are normally
// split one model per channel now, so picking any single channel's list as
// "the" models for the tag would hide every other channel's model from the
// admin (and, via the edit dialogs, invite resending it as an overwrite that
// silently drops the other channels' routes).
func TestGetTagModelsReturnsUnionAcrossChannels(t *testing.T) {
	db := setupChannelTagControllerTestDB(t)
	createTaggedTestChannel(t, db, "prod-pool", "channel-a", "gpt-4o")
	createTaggedTestChannel(t, db, "prod-pool", "channel-b", "claude-3-opus")
	createTaggedTestChannel(t, db, "prod-pool", "channel-c", "gpt-4o") // duplicate model must be deduped

	recorder := performGetTagModels("prod-pool")
	require.Equal(t, http.StatusOK, recorder.Code)

	var response tagModelsTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, 3, response.ChannelCount)
	assert.Equal(t, "claude-3-opus,gpt-4o", response.Data)
}

func TestGetTagModelsUnknownTagHasNoModels(t *testing.T) {
	db := setupChannelTagControllerTestDB(t)
	createTaggedTestChannel(t, db, "other-tag", "channel-a", "gpt-4o")

	recorder := performGetTagModels("empty-tag")
	require.Equal(t, http.StatusOK, recorder.Code)

	var response tagModelsTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, 0, response.ChannelCount)
	assert.Equal(t, "", response.Data)
}

func TestGetTagModelsRequiresTag(t *testing.T) {
	setupChannelTagControllerTestDB(t)

	recorder := performGetTagModels("")
	require.Equal(t, http.StatusBadRequest, recorder.Code)

	var response tagModelsTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.False(t, response.Success)
}

func performEditTagChannels(body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("id", 1)
	ctx.Set("role", common.RoleRootUser)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/channel/tag", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	EditTagChannels(ctx)
	return recorder
}

type editTagChannelsTestResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// TestEditTagChannelsOmittingModelsLeavesChannelsUntouched drives the real
// EditTagChannels HTTP handler through gin's ShouldBindJSON - not
// model.EditChannelByTag directly - to guard the seam PR #9 fixed: the tag
// dialogs used to always send "models", so a plain rename or groups-only save
// silently resent a stale pre-filled model list and wiped out every other
// channel's routes. A request body that omits "models" entirely must decode
// to a nil Models pointer and leave every channel sharing the tag untouched.
func TestEditTagChannelsOmittingModelsLeavesChannelsUntouched(t *testing.T) {
	db := setupChannelTagControllerTestDB(t)
	createTaggedTestChannel(t, db, "prod-pool", "channel-a", "gpt-4o")
	createTaggedTestChannel(t, db, "prod-pool", "channel-b", "claude-3-opus")

	recorder := performEditTagChannels(`{"tag":"prod-pool","new_tag":"prod-pool-renamed"}`)
	require.Equal(t, http.StatusOK, recorder.Code)

	var response editTagChannelsTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)

	var stored []model.Channel
	require.NoError(t, db.Where("tag = ?", "prod-pool-renamed").Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.Equal(t, "gpt-4o", stored[0].Models)
	assert.Equal(t, "claude-3-opus", stored[1].Models)
}

// TestEditTagChannelsWithModelsAppliesAndRebuildsAbilities drives the same
// handler with a non-empty "models" value - the "Start from current models"
// opt-in path - and confirms the documented all-or-nothing overwrite still
// applies to every channel sharing the tag, and that each channel's
// abilities are rebuilt from the new model list (old, stale abilities
// removed) rather than left stale alongside the new ones.
func TestEditTagChannelsWithModelsAppliesAndRebuildsAbilities(t *testing.T) {
	db := setupChannelTagControllerTestDB(t)
	createTaggedTestChannel(t, db, "prod-pool", "channel-a", "gpt-4o")
	createTaggedTestChannel(t, db, "prod-pool", "channel-b", "claude-3-opus")

	var channels []model.Channel
	require.NoError(t, db.Where("tag = ?", "prod-pool").Order("id").Find(&channels).Error)
	require.Len(t, channels, 2)
	for _, ch := range channels {
		require.NoError(t, db.Create(&model.Ability{
			Group:     "default",
			Model:     ch.Models,
			ChannelId: ch.Id,
			Enabled:   true,
		}).Error)
	}

	recorder := performEditTagChannels(`{"tag":"prod-pool","models":"gpt-4o-mini"}`)
	require.Equal(t, http.StatusOK, recorder.Code)

	var response editTagChannelsTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)

	var stored []model.Channel
	require.NoError(t, db.Where("tag = ?", "prod-pool").Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	for _, ch := range stored {
		assert.Equal(t, "gpt-4o-mini", ch.Models)

		var abilities []model.Ability
		require.NoError(t, db.Where("channel_id = ?", ch.Id).Find(&abilities).Error)
		require.Len(t, abilities, 1, "stale ability for the old model should be replaced, not appended to")
		assert.Equal(t, "gpt-4o-mini", abilities[0].Model)
		assert.Equal(t, "default", abilities[0].Group)
	}
}
