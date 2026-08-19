package controller

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type tensorGridUserRequest struct {
	Version         int64  `json:"version"`
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
	Status          string `json:"status"`
	Currency        string `json:"currency"`
	FxRateIrtPerUSD string `json:"fx_rate_irt_per_usd"`
}

type tensorGridBalanceAdjustmentRequest struct {
	AmountMinor       int64  `json:"amount_minor"`
	AmountMicroUSD    int64  `json:"amount_microusd"`
	Currency          string `json:"currency"`
	Reason            string `json:"reason"`
	AllowPartialDebit bool   `json:"allow_partial_debit"`
}

type tensorGridTokenCreateRequest struct {
	Name      string `json:"name"`
	ExpiresAt int64  `json:"expires_at"`
}

type tensorGridTokenRevokeBySecretRequest struct {
	Token string `json:"token"`
}

func tensorGridError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	code := "invalid_request"
	switch {
	case errors.Is(err, model.ErrTensorGridAccountNotFound), errors.Is(err, gorm.ErrRecordNotFound):
		status, code = http.StatusNotFound, "account_not_found"
	case errors.Is(err, model.ErrTensorGridCurrencyMismatch):
		status, code = http.StatusConflict, "currency_mismatch"
	case errors.Is(err, model.ErrTensorGridIdempotencyConflict):
		status, code = http.StatusConflict, "idempotency_conflict"
	case strings.Contains(err.Error(), "insufficient"):
		status, code = http.StatusPaymentRequired, "insufficient_credit"
	}
	c.JSON(status, gin.H{"success": false, "code": code, "message": err.Error()})
}

func UpsertTensorGridUser(c *gin.Context) {
	var request tensorGridUserRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	if request.Status != "active" && request.Status != "disabled" {
		tensorGridError(c, errors.New("status must be active or disabled"))
		return
	}
	account, err := model.UpsertTensorGridAccount(
		c.Param("subject"), request.Email, request.DisplayName, request.Currency,
		request.FxRateIrtPerUSD, request.Status == "active", request.Version,
	)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": account})
}

func GetTensorGridCredit(c *gin.Context) {
	snapshot, err := model.GetTensorGridBalance(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": snapshot})
}

func AdjustTensorGridCredit(c *gin.Context) {
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	var request tensorGridBalanceAdjustmentRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	snapshot, created, appliedAmountMinor, appliedAmountMicroUSD, err := model.AdjustTensorGridBalance(
		c.Param("subject"), idempotencyKey, request.Currency, request.AmountMinor,
		request.AmountMicroUSD, request.Reason, request.AllowPartialDebit,
	)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"success": true, "created": created, "data": snapshot,
		"applied_amount_minor":    appliedAmountMinor,
		"applied_amount_microusd": appliedAmountMicroUSD,
	})
}

func tensorGridTokenResponse(token *model.Token, reveal bool) gin.H {
	key := "sk-" + token.GetMaskedKey()
	if reveal {
		key = "sk-" + token.GetFullKey()
	}
	return gin.H{
		"id": strconv.Itoa(token.Id), "name": token.Name, "key": key,
		"status": token.Status, "created_at": token.CreatedTime,
		"last_used_at": token.AccessedTime, "expires_at": token.ExpiredTime,
	}
}

func ListTensorGridTokens(c *gin.Context) {
	account, err := model.GetTensorGridAccount(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	pageInfo := common.GetPageQuery(c)
	tokens, err := model.GetAllUserTokens(account.UserId, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		tensorGridError(c, err)
		return
	}
	total, err := model.CountUserTokens(account.UserId)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	items := make([]gin.H, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, tensorGridTokenResponse(token, false))
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"page": pageInfo.Page, "page_size": pageInfo.PageSize, "total": total, "items": items,
	}})
}

func CreateTensorGridToken(c *gin.Context) {
	var request tensorGridTokenCreateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 50 {
		tensorGridError(c, errors.New("token name must contain 1 to 50 characters"))
		return
	}
	if request.ExpiresAt != 0 && request.ExpiresAt <= common.GetTimestamp() {
		tensorGridError(c, errors.New("token expiry must be in the future"))
		return
	}
	token, created, err := model.CreateTensorGridToken(
		c.Param("subject"), strings.TrimSpace(c.GetHeader("Idempotency-Key")),
		request.Name, request.ExpiresAt,
	)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{
		"success": true, "created": created, "data": tensorGridTokenResponse(token, true),
	})
}

func RevealTensorGridToken(c *gin.Context) {
	account, err := model.GetTensorGridAccount(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	tokenID, err := strconv.Atoi(c.Param("token_id"))
	if err != nil {
		tensorGridError(c, errors.New("invalid token id"))
		return
	}
	token, err := model.GetTokenByIds(tokenID, account.UserId)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.JSON(http.StatusOK, gin.H{"success": true, "data": tensorGridTokenResponse(token, true)})
}

func RevokeTensorGridToken(c *gin.Context) {
	account, err := model.GetTensorGridAccount(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	tokenID, err := strconv.Atoi(c.Param("token_id"))
	if err != nil {
		tensorGridError(c, errors.New("invalid token id"))
		return
	}
	if err := model.DeleteTokenById(tokenID, account.UserId); err != nil {
		tensorGridError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func RevokeTensorGridTokenBySecret(c *gin.Context) {
	account, err := model.GetTensorGridAccount(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	var request tensorGridTokenRevokeBySecretRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	key := strings.TrimPrefix(strings.TrimSpace(request.Token), "sk-")
	if key == "" {
		tensorGridError(c, errors.New("token is required"))
		return
	}
	token, err := model.GetTokenByKey(key, true)
	if err != nil || token.UserId != account.UserId {
		tensorGridError(c, gorm.ErrRecordNotFound)
		return
	}
	if err := model.DeleteTokenById(token.Id, account.UserId); err != nil {
		tensorGridError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func RevokeTensorGridTokenBySecretGlobal(c *gin.Context) {
	var request tensorGridTokenRevokeBySecretRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		tensorGridError(c, err)
		return
	}
	key := strings.TrimPrefix(strings.TrimSpace(request.Token), "sk-")
	if key == "" {
		tensorGridError(c, errors.New("token is required"))
		return
	}
	token, err := model.GetTokenByKey(key, true)
	if err != nil {
		tensorGridError(c, gorm.ErrRecordNotFound)
		return
	}
	if err := model.DeleteTokenById(token.Id, token.UserId); err != nil {
		tensorGridError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func TensorGridUsage(c *gin.Context) {
	account, err := model.GetTensorGridAccount(c.Param("subject"))
	if err != nil {
		tensorGridError(c, err)
		return
	}
	pageInfo := common.GetPageQuery(c)
	start, _ := strconv.ParseInt(c.Query("start_timestamp"), 10, 64)
	end, _ := strconv.ParseInt(c.Query("end_timestamp"), 10, 64)
	logs, total, totals, err := model.GetTensorGridUserUsage(
		account, start, end, c.Query("model_name"), c.Query("token_name"),
		pageInfo.GetStartIdx(), pageInfo.GetPageSize(), c.Query("request_id"),
		c.Query("status"), c.Query("source"), c.Query("stream"),
	)
	if err != nil {
		tensorGridError(c, err)
		return
	}
	items := make([]gin.H, 0, len(logs))
	for _, log := range logs {
		other, _ := common.StrToMap(log.Other)
		cacheReadTokens := tensorGridUsageNumber(other["cache_tokens"])
		cacheWriteTokens := tensorGridUsageNumber(other["cache_write_tokens"])
		chargeMicroUSD, _ := model.QuotaToMicroUSDForTensorGrid(log.Quota)
		currency := account.Currency
		fxRate := account.FxRateIrtPerUSD
		if billing, ok := other["tensorgrid_billing"].(map[string]interface{}); ok {
			if value, ok := billing["currency"].(string); ok {
				currency = value
			}
			if value, ok := billing["fx_rate_irt_per_usd"].(string); ok {
				fxRate = value
			}
		}
		billingAccount := *account
		billingAccount.Currency = currency
		billingAccount.FxRateIrtPerUSD = fxRate
		chargeMinor, _ := model.MicroUSDToTensorGridMinor(&billingAccount, chargeMicroUSD)
		status := "succeeded"
		if log.Type == model.LogTypeError {
			status = "failed"
		}
		items = append(items, gin.H{
			"id": log.RequestId, "created_at": log.CreatedAt, "status": status,
			"model": log.ModelName, "token_name": log.TokenName, "is_stream": log.IsStream,
			"duration_ms": log.UseTime * 1000, "input_tokens": log.PromptTokens,
			"output_tokens": log.CompletionTokens, "cache_read_tokens": cacheReadTokens,
			"cache_write_tokens": cacheWriteTokens, "charge_microusd": chargeMicroUSD,
			"charge_minor": chargeMinor, "currency": currency,
			"fx_rate_irt_per_usd": fxRate,
		})
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{
		"page": pageInfo.Page, "page_size": pageInfo.PageSize, "total": total,
		"totals": totals, "items": items,
	}})
}

func tensorGridUsageNumber(value interface{}) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int:
		return int64(typed)
	case int64:
		return typed
	case string:
		parsed, _ := strconv.ParseInt(typed, 10, 64)
		return parsed
	default:
		return 0
	}
}
