package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
		assert.Equal(t, tc.want, IsImageGenerationModel(tc.model), "model %q", tc.model)
	}
}
