package common_test

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
)

func TestWanEndpointsDistinguishImagesFromVideos(t *testing.T) {
	for _, name := range []string{
		"wan2.7-image-pro", "wan2.7-image", "wan2.6-image", "wan2.6-t2i",
		"wan2.5-t2i-preview", "wan2.2-t2i-flash", "wan2.2-t2i-plus",
		"wanx2.1-t2i-turbo", "wanx2.1-t2i-plus", "wanx2.0-t2i-turbo",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, common.GetEndpointTypesByChannelType(constant.ChannelTypeAli, name), constant.EndpointTypeImageGeneration)
		})
	}
	for _, name := range []string{
		"wanx2.1-t2v-plus", "wanx2.1-t2v-turbo", "wanx2.1-i2v-plus", "wanx2.1-i2v-turbo",
	} {
		t.Run(name, func(t *testing.T) {
			assert.NotContains(t, common.GetEndpointTypesByChannelType(constant.ChannelTypeAli, name), constant.EndpointTypeImageGeneration)
		})
	}
}

// TestIsImageGenerationModel guards the classification that decides whether a
// model is published with the image-generation endpoint type
// (GetEndpointTypesByChannelType -> model/pricing.go SupportedEndpointTypes).
// The list previously held the literal "gpt-image-1", so every later OpenAI
// image model reached the catalog without that endpoint type.
func TestIsImageGenerationModel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-image-1", true},
		{"gpt-image-1-mini", true},
		{"gpt-image-1.5", true},
		{"gpt-image-2", true},
		{"chatgpt-image-latest", true},
		{"GPT-Image-2", true},
		{"dall-e-2", true},
		{"dall-e-3", true},
		{"imagen-3.0-generate-002", true},
		{"flux-pro", true},
		{"flux.1-dev", true},
		{"gpt-4o", false},
		{"gpt-5.6", false},
		{"text-embedding-3-large", false},
	}

	for _, tc := range cases {
		assert.Equal(t, tc.want, common.IsImageGenerationModel(tc.model), "model %q", tc.model)
	}
}
