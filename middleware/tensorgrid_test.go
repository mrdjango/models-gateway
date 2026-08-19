package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func tensorGridTestSignature(secret, timestamp, method, requestURI, body string) string {
	bodyHash := sha256.Sum256([]byte(body))
	canonical := timestamp + "\n" + method + "\n" + requestURI + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestTensorGridServiceAuthValidatesSignedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "01234567890123456789012345678901"
	t.Setenv("TENSORGRID_INTEGRATION_SECRET", secret)
	const path = "/api/internal/tensorgrid/v1/users/subject"
	const body = `{"currency":"USD"}`
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	router := gin.New()
	router.POST(path, TensorGridServiceAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("X-TensorGrid-Timestamp", timestamp)
	request.Header.Set("X-TensorGrid-Signature", tensorGridTestSignature(secret, timestamp, http.MethodPost, path, body))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusNoContent, recorder.Code)

	request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("X-TensorGrid-Timestamp", timestamp)
	request.Header.Set("X-TensorGrid-Signature", "sha256=invalid")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestTensorGridServiceAuthCoversQueryString(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "01234567890123456789012345678901"
	t.Setenv("TENSORGRID_INTEGRATION_SECRET", secret)
	const path = "/api/internal/tensorgrid/v1/users/subject/usage"
	const signedURI = path + "?page=1"
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	router := gin.New()
	router.GET(path, TensorGridServiceAuth(), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	request := httptest.NewRequest(http.MethodGet, path+"?page=2", nil)
	request.Header.Set("X-TensorGrid-Timestamp", timestamp)
	request.Header.Set("X-TensorGrid-Signature", tensorGridTestSignature(secret, timestamp, http.MethodGet, signedURI, ""))
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusUnauthorized, recorder.Code)
}
