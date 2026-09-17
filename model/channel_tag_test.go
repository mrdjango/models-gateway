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
package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupChannelTagTest(t *testing.T) {
	t.Helper()
	truncateTables(t)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)

	memoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = memoryCacheEnabled
	})
}

func createTaggedChannel(t *testing.T, tag, name, models string) {
	t.Helper()
	channel := Channel{
		Name:   name,
		Key:    "key-" + name,
		Status: common.ChannelStatusEnabled,
		Models: models,
		Group:  "default",
		Tag:    &tag,
	}
	require.NoError(t, DB.Create(&channel).Error)
}

// TestEditChannelByTagPreservesModelsWhenOmitted guards the regression behind
// the tag-edit dialogs wiping out routing after the 2026-09-17 per-model
// channel split: since channels in a tag now normally each carry a single,
// different model, editing an unrelated field (like renaming the tag) must
// never touch any channel's Models.
func TestEditChannelByTagPreservesModelsWhenOmitted(t *testing.T) {
	setupChannelTagTest(t)
	tag := "prod-pool"
	createTaggedChannel(t, tag, "channel-a", "gpt-4o")
	createTaggedChannel(t, tag, "channel-b", "claude-3-opus")

	newTag := "prod-pool-renamed"
	require.NoError(t, EditChannelByTag(tag, &newTag, nil, nil, nil, nil, nil, nil, nil))

	var stored []Channel
	require.NoError(t, DB.Where("tag = ?", newTag).Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.Equal(t, "gpt-4o", stored[0].Models)
	assert.Equal(t, "claude-3-opus", stored[1].Models)
}

// TestEditChannelByTagEmptyModelsIsNoOp documents that an explicit empty
// string behaves the same as a nil pointer and is not applied, matching the
// frontend's assumption that sending "" for an untouched field is safe.
func TestEditChannelByTagEmptyModelsIsNoOp(t *testing.T) {
	setupChannelTagTest(t)
	tag := "prod-pool"
	createTaggedChannel(t, tag, "channel-a", "gpt-4o")

	empty := ""
	require.NoError(t, EditChannelByTag(tag, nil, nil, &empty, nil, nil, nil, nil, nil))

	var stored Channel
	require.NoError(t, DB.Where("tag = ?", tag).First(&stored).Error)
	assert.Equal(t, "gpt-4o", stored.Models)
}

// TestEditChannelByTagAppliesModelsToEveryChannelInTag documents the
// intended (but dangerous-if-unintended) contract: a non-empty models value
// overwrites EVERY channel sharing the tag, not just one. This is exactly
// what an accidentally-resent pre-filled value used to trigger, and it's the
// behavior the "Start from current models" UI flow now opts into explicitly.
func TestEditChannelByTagAppliesModelsToEveryChannelInTag(t *testing.T) {
	setupChannelTagTest(t)
	tag := "prod-pool"
	createTaggedChannel(t, tag, "channel-a", "gpt-4o")
	createTaggedChannel(t, tag, "channel-b", "claude-3-opus")

	overwrite := "gpt-4o-mini"
	require.NoError(t, EditChannelByTag(tag, nil, nil, &overwrite, nil, nil, nil, nil, nil))

	var stored []Channel
	require.NoError(t, DB.Where("tag = ?", tag).Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	for _, ch := range stored {
		assert.Equal(t, "gpt-4o-mini", ch.Models)
	}
}
