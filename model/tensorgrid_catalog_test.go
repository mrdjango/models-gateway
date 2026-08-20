package model

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tensorGridCatalogTestRequest() TensorGridCatalogImport {
	return TensorGridCatalogImport{
		Generation: "legacy-model-hub-v1-test",
		Groups: []TensorGridCatalogImportGroup{{
			ProviderAlias: "tg-go", ProviderName: "TensorGrid Upstream",
			GroupSlug: "auto", GroupName: "Auto", BaseURL: "https://upstream.example.test/",
			Secret: "provider-secret",
			Models: []TensorGridCatalogImportModel{
				{
					PublicID: "tg-go:auto:cached-model", UpstreamModelID: "cached-model",
					DisplayName: "Cached Model", Category: "language",
					EndpointTypes:           []string{"chat.completions", "responses"},
					InputMicroUSDPerMillion: 2_500_000, OutputMicroUSDPerMillion: 10_000_000,
					CacheReadMicroUSDPerMillion: 250_000, CacheWriteMicroUSDPerMillion: 3_125_000,
				},
				{
					PublicID: "tg-go:auto:request-model", UpstreamModelID: "request-model",
					DisplayName: "Request Model", Category: "image",
					EndpointTypes:           []string{"chat.completions"},
					ExtraRetailRates:        map[string]int64{"request": 240_000},
					ExtraRateUnitQuantities: map[string]int64{"request": 1},
				},
			},
		}},
	}
}

func TestImportTensorGridCatalogIsAtomicIdempotentAndExact(t *testing.T) {
	setupTensorGridModelTest(t)
	request := tensorGridCatalogTestRequest()

	result, created, err := ImportTensorGridCatalog(request)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 2, result.ModelCount)
	assert.Equal(t, 1, result.ChannelCount)

	var channel Channel
	require.NoError(t, DB.Where("name = ?", "tensorgrid:tg-go:auto").First(&channel).Error)
	assert.Equal(t, constant.ChannelTypeAdvancedCustom, channel.Type)
	assert.Equal(t, "https://upstream.example.test", channel.GetBaseURL())
	assert.Equal(t, "provider-secret", channel.Key)
	assert.Contains(t, channel.GetModelMapping(), `"tg-go:auto:cached-model":"cached-model"`)
	config := channel.GetOtherSettings().AdvancedCustom
	require.NotNil(t, config)
	assert.True(t, config.SupportsPathForModel("/v1/chat/completions", "tg-go:auto:cached-model"))
	assert.True(t, config.SupportsPathForModel("/v1/responses", "tg-go:auto:cached-model"))
	assert.False(t, config.SupportsPathForModel("/v1/responses", "tg-go:auto:request-model"))

	byName := make(map[string]Pricing)
	for _, pricing := range GetPricing() {
		byName[pricing.ModelName] = pricing
	}
	priced := byName["tg-go:auto:cached-model"]
	assert.Equal(t, "tiered_expr", priced.BillingMode)
	assert.Contains(t, priced.BillingExpr, "cr * (250000 / 1000000)")
	assert.Contains(t, priced.BillingExpr, "cc * (3125000 / 1000000)")
	fixed := byName["tg-go:auto:request-model"]
	assert.Equal(t, 1, fixed.QuotaType)
	assert.InDelta(t, 0.24, fixed.ModelPrice, 0.0000001)

	replayed, replayCreated, err := ImportTensorGridCatalog(request)
	require.NoError(t, err)
	assert.False(t, replayCreated)
	assert.Equal(t, result.CatalogHash, replayed.CatalogHash)
	var channelCount int64
	require.NoError(t, DB.Model(&Channel{}).Where("tag = ?", "tensorgrid-catalog-v1").Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)

	request.Generation = "different-generation"
	_, _, err = ImportTensorGridCatalog(request)
	assert.ErrorIs(t, err, ErrTensorGridCatalogAlreadyImported)
}

func TestPrepareTensorGridCatalogRejectsUnsupportedMeters(t *testing.T) {
	request := tensorGridCatalogTestRequest()
	request.Groups[0].Models[0].ExtraRetailRates = map[string]int64{"unknown_meter": 1}
	request.Groups[0].Models[0].ExtraRateUnitQuantities = map[string]int64{"unknown_meter": 1}

	_, err := prepareTensorGridCatalogImport(request)
	assert.ErrorContains(t, err, "unsupported TensorGrid pricing meter")
}
