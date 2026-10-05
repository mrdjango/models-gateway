package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A JSON body sent to a channel that only takes multipart uploads is the
// caller's mistake: it used to surface as a 500 count_token_failed.
func TestPrepareRequestBillingAnswers400ForUnusableAudioInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := constant.CountToken
	constant.CountToken = true
	t.Cleanup(func() { constant.CountToken = previous })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"whisper-1"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	info := &relaycommon.RelayInfo{
		RelayMode: relayconstant.RelayModeAudioTranscription,
		Request:   &dto.AudioRequest{Model: "whisper-1"},
	}

	apiErr := PrepareRequestBilling(c, info)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, types.ErrorCodeCountTokenFailed, apiErr.GetErrorCode())
	assert.Contains(t, apiErr.Error(), "error parsing multipart form")
}
