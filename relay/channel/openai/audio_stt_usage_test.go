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
