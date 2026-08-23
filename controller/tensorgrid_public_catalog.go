package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

// TensorGridCatalog exposes only the public capability and retail-pricing
// projection. Provider identity, upstream routing, credentials and wholesale
// rates remain Gateway-internal.
func tensorGridPublicEndpoint(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "openai":
		return "chat.completions"
	case "openai-response":
		return "responses"
	case "openai-response-compact":
		return "responses.compact"
	case "openai-alpha-search":
		return "search"
	case "anthropic":
		return "messages"
	case "gemini":
		return "generateContent"
	case "image-generation":
		return "images.generations"
	case "embeddings":
		return "embeddings"
	case "jina-rerank":
		return "rerank"
	case "openai-video":
		return "videos"
	case "openai-audio-transcription":
		return "audio.transcriptions"
	default:
		return ""
	}
}

func tensorGridPublicPricing(row model.Pricing) gin.H {
	pricing := gin.H{
		"input_per_million_microusd":  int64(0),
		"output_per_million_microusd": int64(0),
		"extra_meters":                gin.H{},
	}
	groupRatio := ratio_setting.GetGroupRatio("default")
	if row.BillingMode == billing_setting.BillingModeTieredExpr && strings.TrimSpace(row.BillingExpr) != "" {
		used := billingexpr.UsedVars(row.BillingExpr)
		rate := func(params billingexpr.TokenParams) float64 {
			value, _, err := billingexpr.RunExpr(row.BillingExpr, params)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return 0
			}
			return value * groupRatio
		}
		input := rate(billingexpr.TokenParams{P: 1, Len: 1})
		output := rate(billingexpr.TokenParams{C: 1, Len: 1})
		pricing["input_per_million_microusd"] = tensorGridMicroUSD(input)
		pricing["output_per_million_microusd"] = tensorGridMicroUSD(output)
		if used["cr"] {
			pricing["cache_read_per_million_microusd"] = tensorGridMicroUSD(rate(billingexpr.TokenParams{CR: 1, Len: 1}))
		} else {
			pricing["cache_read_per_million_microusd"] = tensorGridMicroUSD(input)
		}
		if used["cc"] {
			pricing["cache_write_per_million_microusd"] = tensorGridMicroUSD(rate(billingexpr.TokenParams{CC: 1, Len: 1}))
		} else {
			pricing["cache_write_per_million_microusd"] = tensorGridMicroUSD(input)
		}
		meters := pricing["extra_meters"].(gin.H)
		for name, variable := range map[string]string{
			"cache_write_1h": "cc1h", "image_input": "img", "image_output": "img_o",
			"audio_input": "ai", "audio_output": "ao",
		} {
			if !used[variable] {
				continue
			}
			params := billingexpr.TokenParams{Len: 1}
			switch variable {
			case "cc1h":
				params.CC1h = 1
			case "img":
				params.Img = 1
			case "img_o":
				params.ImgO = 1
			case "ai":
				params.AI = 1
			case "ao":
				params.AO = 1
			}
			meters[name] = tensorGridMicroUSD(rate(params))
		}
		return pricing
	}
	if row.QuotaType == 1 {
		pricing["extra_meters"].(gin.H)["request"] = tensorGridMicroUSD(row.ModelPrice * groupRatio)
		return pricing
	}
	classicDollarsPerMillion := 1_000_000 / common.QuotaPerUnit
	input := row.ModelRatio * groupRatio * classicDollarsPerMillion
	pricing["input_per_million_microusd"] = tensorGridMicroUSD(input)
	pricing["output_per_million_microusd"] = tensorGridMicroUSD(input * row.CompletionRatio)
	if row.CacheRatio != nil {
		pricing["cache_read_per_million_microusd"] = tensorGridMicroUSD(input * *row.CacheRatio)
	}
	if row.CreateCacheRatio != nil {
		pricing["cache_write_per_million_microusd"] = tensorGridMicroUSD(input * *row.CreateCacheRatio)
	}
	meters := pricing["extra_meters"].(gin.H)
	if row.ImageRatio != nil {
		meters["image_input"] = tensorGridMicroUSD(input * *row.ImageRatio)
	}
	if row.AudioRatio != nil {
		meters["audio_input"] = tensorGridMicroUSD(input * *row.AudioRatio)
	}
	if row.AudioCompletionRatio != nil && row.AudioRatio != nil {
		meters["audio_output"] = tensorGridMicroUSD(input * *row.AudioRatio * *row.AudioCompletionRatio)
	}
	return pricing
}

func tensorGridMicroUSD(dollarsPerMillion float64) int64 {
	if math.IsNaN(dollarsPerMillion) || math.IsInf(dollarsPerMillion, 0) || dollarsPerMillion <= 0 {
		return 0
	}
	value := dollarsPerMillion * 1_000_000
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(math.Round(value))
}

// tensorGridPublicModalities 推导模型实际接受与产出的模态，供前端按输入类型筛选。
// 端点决定基础模态；只有对话类端点才用计价倍率补充多模态输入，
// 因为像语音转写这类端点的模态由端点本身唯一确定，
// 此时残留的 image/audio 倍率并不代表该端点真的接受这些输入。
func tensorGridPublicModalities(row model.Pricing, endpoints []string) ([]string, []string) {
	inputs := make([]string, 0, 3)
	outputs := make([]string, 0, 2)
	add := func(list []string, value string) []string {
		for _, existing := range list {
			if existing == value {
				return list
			}
		}
		return append(list, value)
	}

	for _, endpoint := range endpoints {
		switch endpoint {
		case "audio.transcriptions":
			inputs = add(inputs, "audio")
			outputs = add(outputs, "text")
		case "images.generations":
			inputs = add(inputs, "text")
			outputs = add(outputs, "image")
		case "videos":
			inputs = add(inputs, "text")
			outputs = add(outputs, "video")
		case "embeddings":
			inputs = add(inputs, "text")
			outputs = add(outputs, "embedding")
		default:
			// 对话类端点：文本必有，图像与音频取决于是否配置了对应倍率。
			inputs = add(inputs, "text")
			outputs = add(outputs, "text")
			if row.ImageRatio != nil {
				inputs = add(inputs, "image")
			}
			if row.AudioRatio != nil {
				inputs = add(inputs, "audio")
			}
			if row.AudioCompletionRatio != nil {
				outputs = add(outputs, "audio")
			}
		}
	}
	return inputs, outputs
}

func tensorGridPublicModel(row model.Pricing) gin.H {
	endpoints := make([]string, 0, len(row.SupportedEndpointTypes))
	capabilities := gin.H{}
	category := "language"
	for _, endpointType := range row.SupportedEndpointTypes {
		endpoint := tensorGridPublicEndpoint(string(endpointType))
		if endpoint == "" {
			continue
		}
		seen := false
		for _, existing := range endpoints {
			if existing == endpoint {
				seen = true
				break
			}
		}
		if !seen {
			endpoints = append(endpoints, endpoint)
		}
		switch endpoint {
		case "embeddings":
			capabilities["embeddings"] = true
			category = "embeddings"
		case "images.generations":
			capabilities["image"] = true
			category = "image"
		case "videos":
			capabilities["video"] = true
			category = "video"
		case "rerank":
			capabilities["rerank"] = true
			category = "rerank"
		case "audio.transcriptions":
			// 语音转写模型的输入是音频、输出是文本，不能落入默认的 text 分类。
			capabilities["audio"] = true
			category = "transcription"
		default:
			capabilities["text"] = true
		}
	}
	inputModalities, outputModalities := tensorGridPublicModalities(row, endpoints)
	return gin.H{
		"id":                row.ModelName,
		"name":              row.ModelName,
		"category":          category,
		"capabilities":      capabilities,
		"endpoints":         endpoints,
		"input_modalities":  inputModalities,
		"output_modalities": outputModalities,
		"pricing":           tensorGridPublicPricing(row),
	}
}

func TensorGridCatalog(c *gin.Context) {
	pricing := model.GetPricing()
	models := make([]gin.H, 0, len(pricing))
	for _, row := range pricing {
		models = append(models, tensorGridPublicModel(row))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i]["id"].(string) < models[j]["id"].(string)
	})
	payload, _ := common.Marshal(models)
	digest := sha256.Sum256(payload)
	revision := hex.EncodeToString(digest[:])
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"revision": revision,
		"models":   models,
	}})
}
