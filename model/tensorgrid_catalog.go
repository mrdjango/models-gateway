package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"gorm.io/gorm"
)

const tensorGridManagedChannelRemark = "tensorgrid-managed"

var tensorGridCatalogSyncMu sync.Mutex

type TensorGridCatalogSync struct {
	Id             int64     `json:"id" gorm:"primaryKey"`
	IdempotencyKey string    `json:"idempotency_key" gorm:"type:varchar(128);not null;uniqueIndex"`
	PayloadSHA256  string    `json:"payload_sha256" gorm:"type:varchar(64);not null"`
	Revision       string    `json:"revision" gorm:"type:varchar(128);not null"`
	ChannelCount   int       `json:"channel_count" gorm:"not null"`
	ModelCount     int       `json:"model_count" gorm:"not null"`
	CreatedAt      time.Time `json:"created_at"`
}

func (TensorGridCatalogSync) TableName() string { return "tensorgrid_catalog_syncs" }

type TensorGridCatalogModel struct {
	PublicId        string `json:"public_id"`
	UpstreamModelId string `json:"upstream_model_id"`
	BillingExpr     string `json:"billing_expr"`
}

type TensorGridCatalogChannel struct {
	ExternalId string                   `json:"external_id"`
	BaseURL    string                   `json:"base_url"`
	Key        string                   `json:"key"`
	Enabled    bool                     `json:"enabled"`
	Models     []TensorGridCatalogModel `json:"models"`
}

type TensorGridCatalogSyncResult struct {
	Revision     string `json:"revision"`
	ChannelCount int    `json:"channel_count"`
	ModelCount   int    `json:"model_count"`
}

func tensorGridChannelTag(externalId string) string {
	digest := sha256.Sum256([]byte(externalId))
	return "tensorgrid-" + hex.EncodeToString(digest[:12])
}

func validateTensorGridCatalogChannel(channel TensorGridCatalogChannel) error {
	channel.ExternalId = strings.TrimSpace(channel.ExternalId)
	if channel.ExternalId == "" || len(channel.ExternalId) > 255 {
		return errors.New("catalog channel external_id must contain 1 to 255 characters")
	}
	parsedURL, err := url.Parse(strings.TrimSpace(channel.BaseURL))
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
		return errors.New("catalog channel base_url must be an HTTPS URL without user information")
	}
	if strings.TrimSpace(channel.Key) == "" {
		return errors.New("catalog channel key is required")
	}
	if len(channel.Models) == 0 {
		return errors.New("catalog channel must contain at least one model")
	}
	seenModels := make(map[string]struct{}, len(channel.Models))
	for _, item := range channel.Models {
		publicId := strings.TrimSpace(item.PublicId)
		upstreamModelId := strings.TrimSpace(item.UpstreamModelId)
		if publicId == "" || len(publicId) > 255 || strings.Contains(publicId, ",") {
			return errors.New("catalog public_id must contain 1 to 255 characters and cannot contain commas")
		}
		if upstreamModelId == "" || len(upstreamModelId) > 255 {
			return errors.New("catalog upstream_model_id must contain 1 to 255 characters")
		}
		if _, exists := seenModels[publicId]; exists {
			return errors.New("catalog contains a duplicate public_id in one channel")
		}
		seenModels[publicId] = struct{}{}
		if err := billing_setting.SmokeTestExpr(strings.TrimSpace(item.BillingExpr)); err != nil {
			return err
		}
	}
	return nil
}

func SyncTensorGridCatalog(
	idempotencyKey, payloadSHA256, revision string,
	channels []TensorGridCatalogChannel,
) (*TensorGridCatalogSyncResult, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	payloadSHA256 = strings.ToLower(strings.TrimSpace(payloadSHA256))
	revision = strings.TrimSpace(revision)
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		return nil, false, errors.New("invalid idempotency key")
	}
	if len(payloadSHA256) != 64 {
		return nil, false, errors.New("invalid catalog payload digest")
	}
	if revision == "" || len(revision) > 128 {
		return nil, false, errors.New("catalog revision must contain 1 to 128 characters")
	}
	if len(channels) == 0 {
		return nil, false, errors.New("catalog must contain at least one channel")
	}
	seenChannels := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		externalId := strings.TrimSpace(channel.ExternalId)
		if _, exists := seenChannels[externalId]; exists {
			return nil, false, errors.New("catalog contains a duplicate channel external_id")
		}
		seenChannels[externalId] = struct{}{}
		if err := validateTensorGridCatalogChannel(channel); err != nil {
			return nil, false, err
		}
	}

	tensorGridCatalogSyncMu.Lock()
	defer tensorGridCatalogSyncMu.Unlock()

	var existingSync TensorGridCatalogSync
	if err := DB.Where("idempotency_key = ?", idempotencyKey).First(&existingSync).Error; err == nil {
		if existingSync.PayloadSHA256 != payloadSHA256 {
			return nil, false, ErrTensorGridIdempotencyConflict
		}
		return &TensorGridCatalogSyncResult{
			Revision: existingSync.Revision, ChannelCount: existingSync.ChannelCount,
			ModelCount: existingSync.ModelCount,
		}, false, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, err
	}

	oldManagedModels := make(map[string]struct{})
	newExpressions := make(map[string]string)
	channelCount := 0
	modelCount := 0
	err := DB.Transaction(func(tx *gorm.DB) error {
		var existingChannels []Channel
		if err := tx.Where("remark = ?", tensorGridManagedChannelRemark).Find(&existingChannels).Error; err != nil {
			return err
		}
		existingByTag := make(map[string]*Channel, len(existingChannels))
		for index := range existingChannels {
			channel := &existingChannels[index]
			if channel.Tag != nil {
				existingByTag[*channel.Tag] = channel
			}
			for _, modelName := range strings.Split(channel.Models, ",") {
				modelName = strings.TrimSpace(modelName)
				if modelName != "" {
					oldManagedModels[modelName] = struct{}{}
				}
			}
		}

		activeTags := make(map[string]struct{}, len(channels))
		for _, input := range channels {
			tag := tensorGridChannelTag(strings.TrimSpace(input.ExternalId))
			activeTags[tag] = struct{}{}
			modelNames := make([]string, 0, len(input.Models))
			modelMapping := make(map[string]string, len(input.Models))
			for _, item := range input.Models {
				publicId := strings.TrimSpace(item.PublicId)
				modelNames = append(modelNames, publicId)
				modelMapping[publicId] = strings.TrimSpace(item.UpstreamModelId)
				newExpressions[publicId] = strings.TrimSpace(item.BillingExpr)
				modelCount++
			}
			sort.Strings(modelNames)
			mappingJSON, err := common.Marshal(modelMapping)
			if err != nil {
				return err
			}
			baseURL := strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
			mapping := string(mappingJSON)
			priority := int64(0)
			weight := uint(100)
			autoBan := 0
			remark := tensorGridManagedChannelRemark
			status := common.ChannelStatusManuallyDisabled
			if input.Enabled {
				status = common.ChannelStatusEnabled
			}
			channel := Channel{
				Type: constant.ChannelTypeOpenAI, Key: strings.TrimSpace(input.Key),
				Status: status, Name: "TensorGrid managed " + strings.TrimPrefix(tag, "tensorgrid-"),
				Weight: &weight, CreatedTime: common.GetTimestamp(), BaseURL: &baseURL,
				Models: strings.Join(modelNames, ","), Group: "default",
				ModelMapping: &mapping, Priority: &priority, AutoBan: &autoBan,
				Tag: &tag, Remark: &remark,
			}
			if existing := existingByTag[tag]; existing != nil {
				channel.Id = existing.Id
				channel.CreatedTime = existing.CreatedTime
				if err := tx.Model(&Channel{}).Where("id = ?", existing.Id).Select(
					"type", "key", "status", "name", "weight", "base_url", "models", "group",
					"model_mapping", "priority", "auto_ban", "tag", "remark",
				).Updates(&channel).Error; err != nil {
					return err
				}
				if err := channel.UpdateAbilities(tx); err != nil {
					return err
				}
			} else {
				if err := tx.Create(&channel).Error; err != nil {
					return err
				}
				if err := channel.AddAbilities(tx); err != nil {
					return err
				}
			}
			channelCount++
		}

		for _, channel := range existingChannels {
			if channel.Tag == nil {
				continue
			}
			if _, active := activeTags[*channel.Tag]; active {
				continue
			}
			if err := tx.Model(&Channel{}).Where("id = ?", channel.Id).
				Update("status", common.ChannelStatusManuallyDisabled).Error; err != nil {
				return err
			}
			if err := tx.Model(&Ability{}).Where("channel_id = ?", channel.Id).
				Update("enabled", false).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}

	billingModes := billing_setting.GetBillingModeCopy()
	billingExpressions := billing_setting.GetBillingExprCopy()
	for modelName := range oldManagedModels {
		delete(billingModes, modelName)
		delete(billingExpressions, modelName)
	}
	for modelName, expression := range newExpressions {
		billingModes[modelName] = billing_setting.BillingModeTieredExpr
		billingExpressions[modelName] = expression
	}
	modeJSON, err := common.Marshal(billingModes)
	if err != nil {
		return nil, false, err
	}
	expressionJSON, err := common.Marshal(billingExpressions)
	if err != nil {
		return nil, false, err
	}
	if err := UpdateOptionsBulk(map[string]string{
		"billing_setting.billing_mode": string(modeJSON),
		"billing_setting.billing_expr": string(expressionJSON),
	}); err != nil {
		return nil, false, err
	}
	InitChannelCache()

	syncRecord := TensorGridCatalogSync{
		IdempotencyKey: idempotencyKey, PayloadSHA256: payloadSHA256, Revision: revision,
		ChannelCount: channelCount, ModelCount: modelCount,
	}
	if err := DB.Create(&syncRecord).Error; err != nil {
		return nil, false, err
	}
	return &TensorGridCatalogSyncResult{
		Revision: revision, ChannelCount: channelCount, ModelCount: modelCount,
	}, true, nil
}
