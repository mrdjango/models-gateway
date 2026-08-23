package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSTTResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// 上游只回 usage.seconds（例如 grok-stt、qwen3-asr）时，按上游时长计费而非本地估算。
func TestOpenaiSTTHandlerUsage(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		estimate      int
		expectedTotal int
	}{
		{
			name:          "upstream tokens win",
			body:          `{"text":"hi","usage":{"total_tokens":395,"input_tokens":3,"output_tokens":17}}`,
			estimate:      999,
			expectedTotal: 395,
		},
		{
			name:          "seconds only falls back to duration billing",
			body:          `{"text":"hi","usage":{"seconds":60,"cost":0.000175}}`,
			estimate:      999,
			expectedTotal: 1000,
		},
		{
			name:          "fractional seconds round up per minute",
			body:          `{"text":"hi","usage":{"seconds":5.994,"cost":0.0001665}}`,
			estimate:      999,
			expectedTotal: 100,
		},
		{
			name:          "no usage keeps local estimate",
			body:          `{"text":"hi"}`,
			estimate:      42,
			expectedTotal: 42,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", nil)

			info := &relaycommon.RelayInfo{}
			info.SetEstimatePromptTokens(tc.estimate)

			apiErr, usage := OpenaiSTTHandler(c, newSTTResponse(tc.body), info, "")
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, tc.expectedTotal, usage.TotalTokens)
		})
	}
}

// 上游的 usage.cost 是采购价，绝不能透传给调用方。
func TestStripUpstreamCost(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		expectStripped bool
	}{
		{
			name:           "openrouter cost is removed",
			body:           `{"text":"hi","usage":{"seconds":5,"cost":0.000175}}`,
			expectStripped: true,
		},
		{
			name:           "token usage without cost is untouched",
			body:           `{"text":"hi","usage":{"total_tokens":395,"input_tokens":3}}`,
			expectStripped: false,
		},
		{
			name:           "plain text response is untouched",
			body:           "سلام میشاد چطوری خوبی؟",
			expectStripped: false,
		},
		{
			name:           "response without usage is untouched",
			body:           `{"text":"hi"}`,
			expectStripped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(stripUpstreamCost([]byte(tc.body)))
			assert.NotContains(t, out, `"cost"`)
			if tc.expectStripped {
				// 其余字段必须保留，只有 cost 消失。
				assert.Contains(t, out, `"seconds"`)
				assert.Contains(t, out, `"text"`)
			} else {
				assert.Equal(t, tc.body, out)
			}
		})
	}
}

// 转写必须记为音频输入，否则 AudioHelper 会走文本计费路径，
// 模型上配置的 audio_ratio 永远不会生效。
func TestOpenaiSTTHandlerMarksAudioInput(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "seconds only upstream", body: `{"text":"hi","usage":{"seconds":60,"cost":0.0021}}`},
		{name: "token upstream", body: `{"text":"hi","usage":{"total_tokens":395,"input_tokens":3,"output_tokens":17}}`},
		{name: "no usage at all", body: `{"text":"hi"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/audio/transcriptions", nil)

			info := &relaycommon.RelayInfo{}
			info.SetEstimatePromptTokens(42)

			apiErr, usage := OpenaiSTTHandler(c, newSTTResponse(tc.body), info, "")
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Positive(t, usage.PromptTokens)
			assert.Equal(t, usage.PromptTokens, usage.PromptTokensDetails.AudioTokens,
				"transcription input must be billed through the audio path")
		})
	}
}
