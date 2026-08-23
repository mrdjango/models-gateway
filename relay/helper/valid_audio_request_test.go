package helper

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAudioRequestContext(t *testing.T, channelType int, body string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	return c
}

// 内联音频的 base64 需要在校验阶段规范化，保证计费解码与转发上游用的是同一份数据。
func TestGetAndValidAudioRequestNormalizesInlineAudio(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		expected string
	}{
		{name: "plain base64", data: "AAECAwQF", expected: "AAECAwQF"},
		{name: "data uri prefix", data: "data:audio/wav;base64,AAECAwQF", expected: "AAECAwQF"},
		{name: "embedded newlines", data: "AAEC\\nAwQF", expected: "AAECAwQF"},
		{name: "data uri with newlines", data: "data:audio/mp3;base64,AAEC\\nAwQF", expected: "AAECAwQF"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"qwen/qwen3-asr-flash-2026-02-10","input_audio":{"data":"` + tc.data + `","format":"wav"}}`
			c := newAudioRequestContext(t, constant.ChannelTypeOpenRouter, body)

			req, err := GetAndValidAudioRequest(c, relayconstant.RelayModeAudioTranscription)
			require.NoError(t, err)
			require.NotNil(t, req.InputAudio)
			assert.Equal(t, tc.expected, req.InputAudio.Data)
			// 上游按自身默认返回，不注入 response_format。
			assert.Empty(t, req.ResponseFormat)
		})
	}
}

func TestGetAndValidAudioRequestRejectsIncompleteInlineAudio(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		errContains string
	}{
		{
			name:        "missing format",
			body:        `{"model":"m","input_audio":{"data":"AAECAwQF"}}`,
			errContains: "input_audio.format is required",
		},
		{
			name:        "empty data",
			body:        `{"model":"m","input_audio":{"data":"","format":"wav"}}`,
			errContains: "input_audio.data is required",
		},
		{
			name:        "whitespace only data",
			body:        `{"model":"m","input_audio":{"data":"  \n ","format":"wav"}}`,
			errContains: "input_audio.data is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAudioRequestContext(t, constant.ChannelTypeOpenRouter, tc.body)

			_, err := GetAndValidAudioRequest(c, relayconstant.RelayModeAudioTranscription)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// 非 OpenRouter 渠道保持原有语义：不规范化，且仍然注入默认 response_format。
func TestGetAndValidAudioRequestLeavesOtherChannelsUnchanged(t *testing.T) {
	body := `{"model":"whisper-1","input_audio":{"data":"data:audio/wav;base64,AAECAwQF","format":"wav"}}`
	c := newAudioRequestContext(t, constant.ChannelTypeOpenAI, body)

	req, err := GetAndValidAudioRequest(c, relayconstant.RelayModeAudioTranscription)
	require.NoError(t, err)
	require.NotNil(t, req.InputAudio)
	assert.Equal(t, "data:audio/wav;base64,AAECAwQF", req.InputAudio.Data)
	assert.Equal(t, "json", req.ResponseFormat)
}
