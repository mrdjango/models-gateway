package model_setting

import (
	"bytes"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldPreserveThinkingSuffixExactAndRegex(t *testing.T) {
	settings := GetGlobalSettings()
	original := append([]string(nil), settings.ThinkingModelBlacklist...)
	t.Cleanup(func() { settings.ThinkingModelBlacklist = original })

	assert.True(t, ShouldPreserveThinkingSuffix("kimi-k2-thinking"))
	assert.True(t, ShouldPreserveThinkingSuffix("moonshotai/kimi-k2-thinking"))
	assert.False(t, ShouldPreserveThinkingSuffix("m@sha256:abc"))

	settings.ThinkingModelBlacklist = []string{
		"kimi-k2-thinking",
		"re:[",
		"re:",
		"re:.*@sha256:.*",
	}

	var logged bytes.Buffer
	previous := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = &logged
	t.Cleanup(func() { gin.DefaultErrorWriter = previous })

	assert.True(t, ShouldPreserveThinkingSuffix("kimi-k2-thinking"))
	assert.True(t, ShouldPreserveThinkingSuffix("m@sha256:abc"))
	assert.False(t, ShouldPreserveThinkingSuffix("m@sha256"))
	assert.False(t, ShouldPreserveThinkingSuffix("qwen3-max@thinking:on"))
	require.Contains(t, logged.String(), `invalid thinking_model_blacklist regex "re:["`)
	require.Contains(t, logged.String(), `invalid thinking_model_blacklist regex "re:"`)

	settings.ThinkingModelBlacklist = []string{"re:^beta@"}
	assert.False(t, ShouldPreserveThinkingSuffix("m@sha256:abc"))
	assert.True(t, ShouldPreserveThinkingSuffix("beta@sha256:abc"))
	assert.False(t, ShouldPreserveThinkingSuffix("alpha@sha256:abc"))
}

func TestMaxPromptTokensFor(t *testing.T) {
	settings := GetGlobalSettings()
	original := settings.MaxPromptTokens
	t.Cleanup(func() { settings.MaxPromptTokens = original })

	settings.MaxPromptTokens = nil
	assert.Equal(t, 0, MaxPromptTokensFor("claude-opus-5"))

	settings.MaxPromptTokens = map[string]int{
		"default":       200000,
		"claude-opus-5": 250000,
		"gpt-5.6-terra": 0,
	}
	assert.Equal(t, 250000, MaxPromptTokensFor("claude-opus-5"), "an exact entry wins")
	assert.Equal(t, 250000, MaxPromptTokensFor("  claude-opus-5 "), "names are trimmed")
	assert.Equal(t, 200000, MaxPromptTokensFor("claude-sonnet-5"), "unlisted models fall back to default")
	assert.Equal(t, 0, MaxPromptTokensFor("gpt-5.6-terra"), "an explicit 0 opts out of the default cap")
}
