package relay

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnforcePromptTokenLimit(t *testing.T) {
	settings := model_setting.GetGlobalSettings()
	original := settings.MaxPromptTokens
	t.Cleanup(func() { settings.MaxPromptTokens = original })

	settings.MaxPromptTokens = map[string]int{"claude-opus-5": 250000}

	assert.Nil(t, enforcePromptTokenLimit("claude-opus-5", 250000), "a prompt at the limit is allowed")
	assert.Nil(t, enforcePromptTokenLimit("claude-sonnet-5", 1897697), "an uncapped model is untouched")
	assert.Nil(t, enforcePromptTokenLimit("claude-opus-5", 0), "a disabled token count cannot trip the cap")

	apiErr := enforcePromptTokenLimit("claude-opus-5", 1897697)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "1897697 tokens")
	assert.Contains(t, apiErr.Error(), "250000 token limit")
}
