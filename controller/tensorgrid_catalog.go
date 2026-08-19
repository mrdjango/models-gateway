package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const maxTensorGridCatalogPayloadBytes = 10 << 20

type tensorGridCatalogSyncRequest struct {
	Revision string                           `json:"revision"`
	Channels []model.TensorGridCatalogChannel `json:"channels"`
}

func SyncTensorGridCatalog(c *gin.Context) {
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	body, err := c.GetRawData()
	if err != nil {
		tensorGridError(c, err)
		return
	}
	if len(body) == 0 || len(body) > maxTensorGridCatalogPayloadBytes {
		tensorGridError(c, errors.New("catalog payload must contain 1 byte to 10 MiB"))
		return
	}
	var request tensorGridCatalogSyncRequest
	if err := common.Unmarshal(body, &request); err != nil {
		tensorGridError(c, err)
		return
	}
	digest := sha256.Sum256(body)
	result, created, err := model.SyncTensorGridCatalog(
		idempotencyKey, hex.EncodeToString(digest[:]), request.Revision, request.Channels,
	)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "created": created, "data": result})
}
