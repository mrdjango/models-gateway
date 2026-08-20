package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const tensorGridCatalogImportMarkerKey = "tensorgrid.catalog_import.v1"

var ErrTensorGridCatalogAlreadyImported = errors.New("tensorgrid catalog was already imported with a different generation")

type TensorGridCatalogImport struct {
	Generation string                         `json:"generation"`
	Groups     []TensorGridCatalogImportGroup `json:"groups"`
}

type TensorGridCatalogImportGroup struct {
	ProviderAlias string                         `json:"provider_alias"`
	ProviderName  string                         `json:"provider_name"`
	GroupSlug     string                         `json:"group_slug"`
	GroupName     string                         `json:"group_name"`
	BaseURL       string                         `json:"base_url"`
	Secret        string                         `json:"secret"`
	Models        []TensorGridCatalogImportModel `json:"models"`
}

type TensorGridCatalogImportModel struct {
	PublicID                     string           `json:"public_id"`
	UpstreamModelID              string           `json:"upstream_model_id"`
	DisplayName                  string           `json:"display_name"`
	Category                     string           `json:"category"`
	EndpointTypes                []string         `json:"endpoint_types"`
	InputMicroUSDPerMillion      int64            `json:"input_microusd_per_million"`
	OutputMicroUSDPerMillion     int64            `json:"output_microusd_per_million"`
	CacheReadMicroUSDPerMillion  int64            `json:"cache_read_microusd_per_million"`
	CacheWriteMicroUSDPerMillion int64            `json:"cache_write_microusd_per_million"`
	ExtraRetailRates             map[string]int64 `json:"extra_retail_rates"`
	ExtraRateUnitQuantities      map[string]int64 `json:"extra_rate_unit_quantities"`
}

type TensorGridCatalogImportResult struct {
	Generation   string `json:"generation"`
	CatalogHash  string `json:"catalog_hash"`
	ModelCount   int    `json:"model_count"`
	ChannelCount int    `json:"channel_count"`
}

type tensorGridCatalogImportMarker struct {
	TensorGridCatalogImportResult
	ImportedAt int64 `json:"imported_at"`
}

type tensorGridPreparedCatalog struct {
	result   TensorGridCatalogImportResult
	channels []Channel
	models   []Model
	options  map[string]string
}

func normalizeTensorGridCatalogEndpoint(value string) (string, string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "chat.completions", "openai":
		return "openai", "/v1/chat/completions", nil
	case "responses", "openai responses", "openai-response":
		return "openai-response", "/v1/responses", nil
	case "responses.compact", "openai-response-compact":
		return "openai-response-compact", "/v1/responses/compact", nil
	case "messages", "anthropic":
		return "anthropic", "/v1/messages", nil
	case "generatecontent", "gemini":
		return "gemini", "/v1beta/models/{model}:generateContent", nil
	case "embeddings":
		return "embeddings", "/v1/embeddings", nil
	case "images.generations", "generate image", "image-generation":
		return "image-generation", "/v1/images/generations", nil
	case "images.edits", "edit image":
		return "image-edit", "/v1/images/edits", nil
	default:
		return "", "", fmt.Errorf("unsupported TensorGrid catalog endpoint %q", value)
	}
}

func tensorGridPricingTerm(variable string, rate, unit int64) string {
	if rate <= 0 {
		return ""
	}
	return fmt.Sprintf("%s * (%d / %d)", variable, rate, unit)
}

func tensorGridTieredExpression(row TensorGridCatalogImportModel) (string, bool, error) {
	terms := make([]string, 0, 9)
	for _, item := range []struct {
		variable string
		rate     int64
	}{
		{"p", row.InputMicroUSDPerMillion},
		{"c", row.OutputMicroUSDPerMillion},
		{"cr", row.CacheReadMicroUSDPerMillion},
		{"cc", row.CacheWriteMicroUSDPerMillion},
	} {
		if item.rate < 0 {
			return "", false, errors.New("TensorGrid catalog rates must not be negative")
		}
		if term := tensorGridPricingTerm(item.variable, item.rate, 1_000_000); term != "" {
			terms = append(terms, term)
		}
	}

	requestRate := int64(0)
	for meter, rate := range row.ExtraRetailRates {
		if rate < 0 {
			return "", false, errors.New("TensorGrid catalog rates must not be negative")
		}
		if meter == "request" {
			requestRate = rate
			continue
		}
		variable, ok := map[string]string{
			"cache_write_1h_token": "cc1h",
			"image_input_token":    "img",
			"image_output_token":   "img_o",
			"audio_input_token":    "ai",
			"audio_output_token":   "ao",
		}[meter]
		if !ok {
			return "", false, fmt.Errorf("unsupported TensorGrid pricing meter %q", meter)
		}
		unit := row.ExtraRateUnitQuantities[meter]
		if unit <= 0 {
			return "", false, fmt.Errorf("TensorGrid pricing meter %q requires a positive unit quantity", meter)
		}
		if term := tensorGridPricingTerm(variable, rate, unit); term != "" {
			terms = append(terms, term)
		}
	}
	if requestRate > 0 && len(terms) > 0 {
		return "", false, errors.New("mixed request and metered TensorGrid pricing is not supported")
	}
	if requestRate > 0 {
		return fmt.Sprintf("%d", requestRate), true, nil
	}
	if len(terms) == 0 {
		return "", false, errors.New("TensorGrid catalog model has no retail price")
	}
	return `tier("tensorgrid-cutover", ` + strings.Join(terms, " + ") + ")", false, nil
}

func prepareTensorGridCatalogImport(request TensorGridCatalogImport) (*tensorGridPreparedCatalog, error) {
	request.Generation = strings.TrimSpace(request.Generation)
	if request.Generation == "" || len(request.Generation) > 128 {
		return nil, errors.New("TensorGrid catalog generation must contain 1 to 128 characters")
	}
	if len(request.Groups) == 0 {
		return nil, errors.New("TensorGrid catalog import requires at least one group")
	}
	payload, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	catalogHash := hex.EncodeToString(digest[:])

	modelRatios := make(map[string]float64)
	completionRatios := make(map[string]float64)
	cacheRatios := make(map[string]float64)
	createCacheRatios := make(map[string]float64)
	modelPrices := make(map[string]float64)
	billingModes := make(map[string]string)
	billingExpressions := make(map[string]string)
	seenModels := make(map[string]struct{})
	seenGroups := make(map[string]struct{})
	channels := make([]Channel, 0, len(request.Groups))
	models := make([]Model, 0)
	tag := "tensorgrid-catalog-v1"
	now := time.Now().Unix()

	for _, group := range request.Groups {
		group.ProviderAlias = strings.TrimSpace(group.ProviderAlias)
		group.GroupSlug = strings.TrimSpace(group.GroupSlug)
		group.BaseURL = strings.TrimRight(strings.TrimSpace(group.BaseURL), "/")
		group.Secret = strings.TrimSpace(group.Secret)
		groupKey := group.ProviderAlias + ":" + group.GroupSlug
		if group.ProviderAlias == "" || group.GroupSlug == "" || group.Secret == "" {
			return nil, errors.New("TensorGrid catalog group identity and secret are required")
		}
		if _, exists := seenGroups[groupKey]; exists {
			return nil, fmt.Errorf("duplicate TensorGrid catalog group %q", groupKey)
		}
		seenGroups[groupKey] = struct{}{}
		parsedBaseURL, parseErr := url.Parse(group.BaseURL)
		if parseErr != nil || (parsedBaseURL.Scheme != "https" && parsedBaseURL.Scheme != "http") || parsedBaseURL.Host == "" || parsedBaseURL.User != nil {
			return nil, fmt.Errorf("invalid TensorGrid upstream base URL for group %q", groupKey)
		}
		if len(group.Models) == 0 {
			return nil, fmt.Errorf("TensorGrid catalog group %q has no models", groupKey)
		}

		modelNames := make([]string, 0, len(group.Models))
		modelMapping := make(map[string]string, len(group.Models))
		routeModels := make(map[string][]string)
		for _, row := range group.Models {
			row.PublicID = strings.TrimSpace(row.PublicID)
			row.UpstreamModelID = strings.TrimSpace(row.UpstreamModelID)
			if row.PublicID == "" || len(row.PublicID) > 128 || row.UpstreamModelID == "" {
				return nil, errors.New("TensorGrid catalog model IDs are required and public IDs must fit the Gateway")
			}
			if _, exists := seenModels[row.PublicID]; exists {
				return nil, fmt.Errorf("duplicate TensorGrid public model %q", row.PublicID)
			}
			seenModels[row.PublicID] = struct{}{}
			modelNames = append(modelNames, row.PublicID)
			modelMapping[row.PublicID] = row.UpstreamModelID
			endpointMap := make(map[string]string)
			for _, rawEndpoint := range row.EndpointTypes {
				endpoint, path, endpointErr := normalizeTensorGridCatalogEndpoint(rawEndpoint)
				if endpointErr != nil {
					return nil, endpointErr
				}
				endpointMap[endpoint] = path
				routeModels[path] = append(routeModels[path], row.PublicID)
			}
			if len(endpointMap) == 0 {
				return nil, fmt.Errorf("TensorGrid public model %q has no endpoint", row.PublicID)
			}
			expression, fixedRequest, pricingErr := tensorGridTieredExpression(row)
			if pricingErr != nil {
				return nil, fmt.Errorf("TensorGrid public model %q: %w", row.PublicID, pricingErr)
			}
			if fixedRequest {
				var requestMicroUSD int64
				_, _ = fmt.Sscan(expression, &requestMicroUSD)
				modelPrices[row.PublicID] = float64(requestMicroUSD) / 1_000_000
			} else {
				modelRatios[row.PublicID] = float64(row.InputMicroUSDPerMillion) / 2_000_000
				if row.InputMicroUSDPerMillion > 0 {
					completionRatios[row.PublicID] = float64(row.OutputMicroUSDPerMillion) / float64(row.InputMicroUSDPerMillion)
					cacheRatios[row.PublicID] = float64(row.CacheReadMicroUSDPerMillion) / float64(row.InputMicroUSDPerMillion)
					createCacheRatios[row.PublicID] = float64(row.CacheWriteMicroUSDPerMillion) / float64(row.InputMicroUSDPerMillion)
				}
				billingModes[row.PublicID] = "tiered_expr"
				billingExpressions[row.PublicID] = expression
			}
			endpointsJSON, _ := common.Marshal(endpointMap)
			models = append(models, Model{
				ModelName: row.PublicID, Description: strings.TrimSpace(row.DisplayName),
				Tags: strings.TrimSpace(row.Category), Endpoints: string(endpointsJSON),
				Status: 1, SyncOfficial: 0, CreatedTime: now, UpdatedTime: now,
			})
		}
		sort.Strings(modelNames)
		routes := make([]dto.AdvancedCustomRoute, 0, len(routeModels))
		paths := make([]string, 0, len(routeModels))
		for path := range routeModels {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			routeModelNames := routeModels[path]
			sort.Strings(routeModelNames)
			routes = append(routes, dto.AdvancedCustomRoute{
				IncomingPath: path, UpstreamPath: path, Converter: "none", Models: routeModelNames,
			})
		}
		advancedConfig := &dto.AdvancedCustomConfig{Routes: routes}
		if validateErr := advancedConfig.Validate(); validateErr != nil {
			return nil, fmt.Errorf("invalid TensorGrid routes for group %q: %w", groupKey, validateErr)
		}
		settingsJSON, _ := common.Marshal(dto.ChannelOtherSettings{AdvancedCustom: advancedConfig})
		mappingJSON, _ := common.Marshal(modelMapping)
		baseURL := group.BaseURL
		mapping := string(mappingJSON)
		priority := int64(0)
		weight := uint(0)
		channels = append(channels, Channel{
			Type: constant.ChannelTypeAdvancedCustom, Key: group.Secret,
			Status: common.ChannelStatusEnabled, Name: "tensorgrid:" + groupKey,
			Weight: &weight, CreatedTime: now, BaseURL: &baseURL,
			Models: strings.Join(modelNames, ","), Group: "default",
			ModelMapping: &mapping, Priority: &priority, Tag: &tag,
			OtherSettings: string(settingsJSON),
		})
	}

	optionMaps := map[string]interface{}{
		"ModelRatio": modelRatios, "CompletionRatio": completionRatios,
		"CacheRatio": cacheRatios, "CreateCacheRatio": createCacheRatios,
		"ModelPrice": modelPrices, "billing_setting.billing_mode": billingModes,
		"billing_setting.billing_expr": billingExpressions,
	}
	options := make(map[string]string, len(optionMaps))
	for key, value := range optionMaps {
		encoded, marshalErr := common.Marshal(value)
		if marshalErr != nil {
			return nil, marshalErr
		}
		options[key] = string(encoded)
	}
	return &tensorGridPreparedCatalog{
		result: TensorGridCatalogImportResult{
			Generation: request.Generation, CatalogHash: catalogHash,
			ModelCount: len(seenModels), ChannelCount: len(channels),
		},
		channels: channels, models: models, options: options,
	}, nil
}

func ImportTensorGridCatalog(request TensorGridCatalogImport) (*TensorGridCatalogImportResult, bool, error) {
	prepared, err := prepareTensorGridCatalogImport(request)
	if err != nil {
		return nil, false, err
	}
	created := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		var marker Option
		markerErr := tx.Where("key = ?", tensorGridCatalogImportMarkerKey).First(&marker).Error
		if markerErr == nil {
			var existing tensorGridCatalogImportMarker
			if unmarshalErr := common.Unmarshal([]byte(marker.Value), &existing); unmarshalErr != nil {
				return unmarshalErr
			}
			if existing.Generation != prepared.result.Generation || existing.CatalogHash != prepared.result.CatalogHash {
				return ErrTensorGridCatalogAlreadyImported
			}
			prepared.result = existing.TensorGridCatalogImportResult
			return nil
		}
		if !errors.Is(markerErr, gorm.ErrRecordNotFound) {
			return markerErr
		}

		expectedChannels := make(map[string]struct{}, len(prepared.channels))
		for i := range prepared.channels {
			channel := &prepared.channels[i]
			expectedChannels[channel.Name] = struct{}{}
			var existing Channel
			lookupErr := tx.Where("name = ? AND tag = ?", channel.Name, *channel.Tag).First(&existing).Error
			if lookupErr == nil {
				channel.Id = existing.Id
				channel.CreatedTime = existing.CreatedTime
				if updateErr := tx.Model(&Channel{}).Where("id = ?", existing.Id).Select(
					"type", "key", "status", "name", "weight", "base_url", "models", "group",
					"model_mapping", "priority", "tag", "settings",
				).Updates(channel).Error; updateErr != nil {
					return updateErr
				}
				if updateErr := channel.UpdateAbilities(tx); updateErr != nil {
					return updateErr
				}
				continue
			}
			if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return lookupErr
			}
			if createErr := tx.Create(channel).Error; createErr != nil {
				return createErr
			}
			if abilityErr := channel.AddAbilities(tx); abilityErr != nil {
				return abilityErr
			}
		}
		var staleChannels []Channel
		if queryErr := tx.Where("tag = ?", "tensorgrid-catalog-v1").Find(&staleChannels).Error; queryErr != nil {
			return queryErr
		}
		for i := range staleChannels {
			if _, ok := expectedChannels[staleChannels[i].Name]; ok {
				continue
			}
			if updateErr := tx.Model(&Channel{}).Where("id = ?", staleChannels[i].Id).Update("status", common.ChannelStatusManuallyDisabled).Error; updateErr != nil {
				return updateErr
			}
			if updateErr := tx.Model(&Ability{}).Where("channel_id = ?", staleChannels[i].Id).Update("enabled", false).Error; updateErr != nil {
				return updateErr
			}
		}
		for i := range prepared.models {
			row := &prepared.models[i]
			var existing Model
			lookupErr := tx.Where("model_name = ?", row.ModelName).First(&existing).Error
			if lookupErr == nil {
				if updateErr := tx.Model(&Model{}).Where("id = ?", existing.Id).Updates(map[string]interface{}{
					"description": row.Description, "tags": row.Tags, "endpoints": row.Endpoints,
					"status": 1, "sync_official": 0, "updated_time": row.UpdatedTime,
				}).Error; updateErr != nil {
					return updateErr
				}
				continue
			}
			if !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
				return lookupErr
			}
			if createErr := tx.Create(row).Error; createErr != nil {
				return createErr
			}
		}
		for key, value := range prepared.options {
			option := Option{Key: key, Value: value}
			if optionErr := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value"}),
			}).Create(&option).Error; optionErr != nil {
				return optionErr
			}
		}
		markerPayload, marshalErr := common.Marshal(tensorGridCatalogImportMarker{
			TensorGridCatalogImportResult: prepared.result, ImportedAt: time.Now().Unix(),
		})
		if marshalErr != nil {
			return marshalErr
		}
		if markerErr := tx.Create(&Option{Key: tensorGridCatalogImportMarkerKey, Value: string(markerPayload)}).Error; markerErr != nil {
			return markerErr
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	// Refresh on both the first import and an idempotent replay. This closes the
	// narrow crash window after the database commit but before cache refresh.
	for key, value := range prepared.options {
		if optionErr := updateOptionMap(key, value); optionErr != nil {
			return nil, false, optionErr
		}
	}
	InitChannelCache()
	RefreshPricing()
	result := prepared.result
	return &result, created, nil
}
