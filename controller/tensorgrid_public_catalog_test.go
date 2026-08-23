package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTensorGridPublicCatalogSanitizesPricingAndProviderData(t *testing.T) {
	previousQuotaPerUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500_000
	t.Cleanup(func() { common.QuotaPerUnit = previousQuotaPerUnit })
	cacheRead := 0.1
	cacheWrite := 0.2
	row := model.Pricing{
		ModelName:        "public:gpt-5.5",
		QuotaType:        0,
		ModelRatio:       2,
		CompletionRatio:  3,
		CacheRatio:       &cacheRead,
		CreateCacheRatio: &cacheWrite,
		SupportedEndpointTypes: []constant.EndpointType{
			constant.EndpointTypeOpenAI,
			constant.EndpointTypeOpenAIResponse,
		},
		BillingExpr: `tier("base", p * 2 + c * 6)`,
	}

	public := tensorGridPublicModel(row)
	require.Equal(t, "public:gpt-5.5", public["id"])
	assert.Equal(t, "language", public["category"])
	assert.Equal(t, []string{"chat.completions", "responses"}, public["endpoints"])
	assert.Equal(t, true, public["capabilities"].(gin.H)["text"])
	assert.NotContains(t, public, "provider")
	assert.NotContains(t, public, "base_url")
	assert.NotContains(t, public, "key")
	assert.NotContains(t, public, "model_ratio")
	assert.NotContains(t, public, "billing_expr")

	pricing, ok := public["pricing"].(gin.H)
	require.True(t, ok)
	assert.Equal(t, int64(4_000_000), pricing["input_per_million_microusd"])
	assert.Equal(t, int64(12_000_000), pricing["output_per_million_microusd"])
	assert.Equal(t, int64(400_000), pricing["cache_read_per_million_microusd"])
	assert.Equal(t, int64(800_000), pricing["cache_write_per_million_microusd"])
}

func TestTensorGridPublicCatalogEvaluatesTieredRetailRatesWithoutExposingExpression(t *testing.T) {
	row := model.Pricing{
		ModelName: "tiered:model", QuotaType: 1, ModelPrice: 99, BillingMode: "tiered_expr",
		BillingExpr: `tier("base", p * 2.5 + c * 10 + cr * 0.25 + cc * 3.125 + cc1h * 5 + img * 4 + img_o * 12 + ai * 8 + ao * 16)`,
	}

	public := tensorGridPublicModel(row)
	assert.NotContains(t, public, "billing_expr")
	pricing := public["pricing"].(gin.H)
	assert.Equal(t, int64(2_500_000), pricing["input_per_million_microusd"])
	assert.Equal(t, int64(10_000_000), pricing["output_per_million_microusd"])
	assert.Equal(t, int64(250_000), pricing["cache_read_per_million_microusd"])
	assert.Equal(t, int64(3_125_000), pricing["cache_write_per_million_microusd"])
	meters := pricing["extra_meters"].(gin.H)
	assert.Equal(t, int64(5_000_000), meters["cache_write_1h"])
	assert.Equal(t, int64(4_000_000), meters["image_input"])
	assert.Equal(t, int64(12_000_000), meters["image_output"])
	assert.Equal(t, int64(8_000_000), meters["audio_input"])
	assert.Equal(t, int64(16_000_000), meters["audio_output"])
}

func TestTensorGridPublicCatalogUsesRequestMeterForFixedPricing(t *testing.T) {
	public := tensorGridPublicModel(model.Pricing{
		ModelName:              "image:model",
		QuotaType:              1,
		ModelPrice:             0.04,
		SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeImageGeneration},
	})

	assert.Equal(t, "image", public["category"])
	pricing := public["pricing"].(gin.H)
	assert.Equal(t, int64(40_000), pricing["extra_meters"].(gin.H)["request"])
	assert.Equal(t, int64(0), pricing["input_per_million_microusd"])
}

// 语音转写模型必须以 audio.transcriptions 端点和 transcription 分类对外暴露，
// 否则前端只能把它当作普通 chat 模型展示与筛选。
func TestTensorGridPublicModelTranscription(t *testing.T) {
	row := model.Pricing{
		ModelName: "qwen/qwen3-asr-flash-2026-02-10",
		SupportedEndpointTypes: []constant.EndpointType{
			constant.EndpointTypeOpenAIAudioTranscription,
		},
	}

	public := tensorGridPublicModel(row)
	assert.Equal(t, []string{"audio.transcriptions"}, public["endpoints"])
	assert.Equal(t, "transcription", public["category"])
	assert.Equal(t, true, public["capabilities"].(gin.H)["audio"])
	assert.NotContains(t, public["capabilities"].(gin.H), "text")
}

// 输入模态用于前端按“接受什么类型的输入”筛选，必须由端点决定，
// 而不是由残留的计价倍率决定。
func TestTensorGridPublicModelModalities(t *testing.T) {
	imageRatio := 2.0
	audioRatio := 3.0
	audioCompletionRatio := 1.5

	cases := []struct {
		name            string
		row             model.Pricing
		expectedInputs  []string
		expectedOutputs []string
	}{
		{
			name: "plain chat model is text only",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAI},
			},
			expectedInputs:  []string{"text"},
			expectedOutputs: []string{"text"},
		},
		{
			name: "multimodal chat model reports image and audio input",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAI},
				ImageRatio:             &imageRatio,
				AudioRatio:             &audioRatio,
				AudioCompletionRatio:   &audioCompletionRatio,
			},
			expectedInputs:  []string{"text", "image", "audio"},
			expectedOutputs: []string{"text", "audio"},
		},
		{
			name: "transcription takes audio in and text out",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAIAudioTranscription},
			},
			expectedInputs:  []string{"audio"},
			expectedOutputs: []string{"text"},
		},
		{
			// 回归：ASR 模型上误配的 image 倍率不得把它标成接受图像输入。
			name: "stray image ratio does not leak into a transcription model",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeOpenAIAudioTranscription},
				ImageRatio:             &imageRatio,
			},
			expectedInputs:  []string{"audio"},
			expectedOutputs: []string{"text"},
		},
		{
			name: "image generation takes text in and image out",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeImageGeneration},
			},
			expectedInputs:  []string{"text"},
			expectedOutputs: []string{"image"},
		},
		{
			name: "embeddings report an embedding output",
			row: model.Pricing{
				SupportedEndpointTypes: []constant.EndpointType{constant.EndpointTypeEmbeddings},
			},
			expectedInputs:  []string{"text"},
			expectedOutputs: []string{"embedding"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			public := tensorGridPublicModel(tc.row)
			assert.Equal(t, tc.expectedInputs, public["input_modalities"])
			assert.Equal(t, tc.expectedOutputs, public["output_modalities"])
		})
	}
}
