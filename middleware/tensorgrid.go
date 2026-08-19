package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const tensorGridSignatureMaxAge = 300 * time.Second

func TensorGridServiceAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		secret := os.Getenv("TENSORGRID_INTEGRATION_SECRET")
		if len(secret) < 32 {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "integration_not_configured"})
			return
		}
		timestamp := strings.TrimSpace(c.GetHeader("X-TensorGrid-Timestamp"))
		unixTimestamp, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil || time.Since(time.Unix(unixTimestamp, 0)).Abs() > tensorGridSignatureMaxAge {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "code": "invalid_integration_timestamp"})
			return
		}
		body, err := c.GetRawData()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"success": false, "code": "invalid_request_body"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		bodyHash := sha256.Sum256(body)
		canonical := timestamp + "\n" + c.Request.Method + "\n" + c.Request.URL.RequestURI() + "\n" + hex.EncodeToString(bodyHash[:])
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(canonical))
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		provided := strings.TrimSpace(c.GetHeader("X-TensorGrid-Signature"))
		if !hmac.Equal([]byte(expected), []byte(provided)) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"success": false, "code": "invalid_integration_signature"})
			return
		}
		c.Next()
	}
}
