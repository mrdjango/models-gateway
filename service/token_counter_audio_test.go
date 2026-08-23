package service

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	constant2 "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildPCMWav 生成一个时长为 seconds 的 16-bit 单声道 WAV，用于音频时长解析。
func buildPCMWav(t *testing.T, sampleRate int, seconds int) []byte {
	t.Helper()
	const bitsPerSample = 16
	const channels = 1
	dataLen := sampleRate * seconds * channels * bitsPerSample / 8

	buf := &bytes.Buffer{}
	buf.WriteString("RIFF")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(36+dataLen)))
	buf.WriteString("WAVEfmt ")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(16)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(1)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(channels)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(sampleRate)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(sampleRate*channels*bitsPerSample/8)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(channels*bitsPerSample/8)))
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(bitsPerSample)))
	buf.WriteString("data")
	require.NoError(t, binary.Write(buf, binary.LittleEndian, uint32(dataLen)))
	buf.Write(make([]byte, dataLen))
	return buf.Bytes()
}

func newInlineAudioContext(t *testing.T, channelType int) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousCountToken := constant.CountToken
	constant.CountToken = true
	t.Cleanup(func() { constant.CountToken = previousCountToken })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", bytes.NewReader(nil))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyChannelType, channelType)
	return c
}

// JSON 内联 base64 音频不得再走 multipart 解析，token 数按音频时长计算（每分钟 1000 token）。
func TestEstimateRequestTokenInlineAudioUsesDuration(t *testing.T) {
	cases := []struct {
		name     string
		seconds  int
		format   string
		expected int
	}{
		{name: "one second", seconds: 1, format: "wav", expected: 17},
		{name: "sixty seconds", seconds: 60, format: "wav", expected: 1000},
		{name: "format with leading dot", seconds: 1, format: ".wav", expected: 17},
		{name: "uppercase format", seconds: 1, format: "WAV", expected: 17},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newInlineAudioContext(t, constant.ChannelTypeOpenRouter)
			wav := buildPCMWav(t, 8000, tc.seconds)
			info := &relaycommon.RelayInfo{
				RelayMode: constant2.RelayModeAudioTranscription,
				Request: &dto.AudioRequest{
					Model:      "qwen/qwen3-asr-flash-2026-02-10",
					InputAudio: &dto.AudioInput{Data: base64.StdEncoding.EncodeToString(wav), Format: tc.format},
				},
			}

			tokens, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
			require.NoError(t, err)
			assert.Equal(t, tc.expected, tokens)
		})
	}
}

func TestEstimateRequestTokenInlineAudioRejectsBadData(t *testing.T) {
	c := newInlineAudioContext(t, constant.ChannelTypeOpenRouter)
	info := &relaycommon.RelayInfo{
		RelayMode: constant2.RelayModeAudioTranscription,
		Request: &dto.AudioRequest{
			Model:      "qwen/qwen3-asr-flash-2026-02-10",
			InputAudio: &dto.AudioInput{Data: "not-base64!!!", Format: "wav"},
		},
	}

	_, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error decoding input_audio data")
}

// 内联音频只对 OpenRouter 生效，其他渠道保持 multipart 语义不变。
func TestEstimateRequestTokenInlineAudioOnlyAppliesToOpenRouter(t *testing.T) {
	c := newInlineAudioContext(t, constant.ChannelTypeOpenAI)
	wav := buildPCMWav(t, 8000, 1)
	info := &relaycommon.RelayInfo{
		RelayMode: constant2.RelayModeAudioTranscription,
		Request: &dto.AudioRequest{
			Model:      "whisper-1",
			InputAudio: &dto.AudioInput{Data: base64.StdEncoding.EncodeToString(wav), Format: "wav"},
		},
	}

	_, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "error parsing multipart form")
}

// 计费不变量：无法计量时长的音频必须被拒绝，不能按 0 token 免费放行。
// 声明的格式与实际字节不符时，解析器会静默返回 0 时长。
func TestEstimateRequestTokenRejectsUnmeasurableInlineAudio(t *testing.T) {
	c := newInlineAudioContext(t, constant.ChannelTypeOpenRouter)
	wav := buildPCMWav(t, 8000, 5)
	info := &relaycommon.RelayInfo{
		RelayMode: constant2.RelayModeAudioTranscription,
		Request: &dto.AudioRequest{
			Model:      "qwen/qwen3-asr-flash-2026-02-10",
			InputAudio: &dto.AudioInput{Data: base64.StdEncoding.EncodeToString(wav), Format: "mp3"},
		},
	}

	tokens, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
	require.ErrorIs(t, err, errAudioDurationNotMeasurable)
	assert.Zero(t, tokens)
}

// multipart 上传走同一条计费不变量：扩展名与实际字节不符时同样必须拒绝。
func TestEstimateRequestTokenRejectsUnmeasurableMultipartAudio(t *testing.T) {
	wav := buildPCMWav(t, 8000, 5)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	require.NoError(t, writer.WriteField("model", "whisper-1"))
	part, err := writer.CreateFormFile("file", "audio.mp3")
	require.NoError(t, err)
	_, err = part.Write(wav)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	gin.SetMode(gin.TestMode)
	previousCountToken := constant.CountToken
	constant.CountToken = true
	t.Cleanup(func() { constant.CountToken = previousCountToken })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)

	info := &relaycommon.RelayInfo{
		RelayMode: constant2.RelayModeAudioTranscription,
		Request:   &dto.AudioRequest{Model: "whisper-1"},
	}

	tokens, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
	require.ErrorIs(t, err, errAudioDurationNotMeasurable)
	assert.Zero(t, tokens)
}

// 正常的 multipart 上传仍按时长计费，确保上面的拒绝不会误伤合法请求。
func TestEstimateRequestTokenMultipartAudioUsesDuration(t *testing.T) {
	wav := buildPCMWav(t, 8000, 60)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	require.NoError(t, writer.WriteField("model", "whisper-1"))
	part, err := writer.CreateFormFile("file", "audio.wav")
	require.NoError(t, err)
	_, err = part.Write(wav)
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	gin.SetMode(gin.TestMode)
	previousCountToken := constant.CountToken
	constant.CountToken = true
	t.Cleanup(func() { constant.CountToken = previousCountToken })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)

	info := &relaycommon.RelayInfo{
		RelayMode: constant2.RelayModeAudioTranscription,
		Request:   &dto.AudioRequest{Model: "whisper-1"},
	}

	tokens, err := EstimateRequestToken(c, &types.TokenCountMeta{}, info)
	require.NoError(t, err)
	assert.Equal(t, 1000, tokens)
}
