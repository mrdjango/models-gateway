package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	TensorGridOutboxEvent    = "event"
	TensorGridOutboxSnapshot = "snapshot"
)

var (
	tensorGridReconcileMu     sync.Mutex
	tensorGridReconcileOffset int
)

type TensorGridCreditDelivery struct {
	EventId         string                 `json:"event_id"`
	RequestId       string                 `json:"request_id"`
	SubjectId       string                 `json:"subject_id"`
	Sequence        int64                  `json:"sequence"`
	Currency        string                 `json:"currency"`
	DeltaMinor      int64                  `json:"delta_minor"`
	DeltaMicroUSD   int64                  `json:"delta_microusd"`
	BalanceMinor    int64                  `json:"balance_minor"`
	BalanceMicroUSD int64                  `json:"balance_microusd"`
	UsageBreakdown  map[string]interface{} `json:"usage_breakdown"`
	PricingVersion  string                 `json:"pricing_version"`
	OccurredAt      time.Time              `json:"occurred_at"`
}

func TensorGridCreditDeliveryFromOutbox(row *TensorGridCreditOutbox) (TensorGridCreditDelivery, error) {
	breakdown := map[string]interface{}{}
	if strings.TrimSpace(row.UsageBreakdown) != "" {
		if err := common.Unmarshal([]byte(row.UsageBreakdown), &breakdown); err != nil {
			return TensorGridCreditDelivery{}, err
		}
	}
	return TensorGridCreditDelivery{
		EventId: row.EventId, RequestId: row.RequestId, SubjectId: row.Subject,
		Sequence: row.Sequence, Currency: row.Currency,
		DeltaMinor: row.DeltaMinor, DeltaMicroUSD: row.DeltaMicroUSD,
		BalanceMinor: row.BalanceMinor, BalanceMicroUSD: row.BalanceMicroUSD,
		UsageBreakdown: breakdown, PricingVersion: row.PricingVersion,
		OccurredAt: row.OccurredAt,
	}, nil
}

func tensorGridCreditEventValues(account *TensorGridAccount, params RecordConsumeLogParams) (int64, int64, string, string, error) {
	microUSD, err := quotaToMicroUSD(params.Quota)
	if err != nil {
		return 0, 0, "", "", err
	}
	deltaMinor := int64(0)
	deltaMicroUSD := int64(0)
	if account.Currency == TensorGridCurrencyUSD {
		deltaMicroUSD = -microUSD
		deltaMinor = -(microUSD / 10_000)
	} else {
		deltaMinor, err = MicroUSDToTensorGridMinor(account, -microUSD)
		if err != nil {
			return 0, 0, "", "", err
		}
	}
	// Snapshot flattens the log's public scope back to the top level, which is
	// where every usage key read below is written.
	other := params.Other.Snapshot()
	usage := map[string]interface{}{
		"input_tokens":        params.PromptTokens,
		"output_tokens":       params.CompletionTokens,
		"cache_read_tokens":   tensorGridLogNumber(other, "cache_tokens"),
		"cache_write_tokens":  tensorGridLogNumber(other, "cache_write_tokens"),
		"image_input_tokens":  tensorGridFirstLogNumber(other, "image_input_tokens", "image_output"),
		"image_output_tokens": tensorGridLogNumber(other, "image_output_tokens"),
		"audio_input_tokens":  tensorGridFirstLogNumber(other, "audio_input_tokens", "audio_input_token_count", "audio_input"),
		"audio_output_tokens": tensorGridFirstLogNumber(other, "audio_output_tokens", "audio_output"),
		"request_path":        tensorGridLogString(other, "request_path"),
	}
	for _, key := range []string{"billed_quota", "collected_quota", "uncollected_quota"} {
		if value, ok := other[key]; ok {
			usage[key] = value
		}
	}
	if matchedTier := tensorGridLogString(other, "matched_tier"); matchedTier != "" {
		usage["matched_tier"] = matchedTier
	}
	pricingVersion := tensorGridPricingVersion(params.ModelName, other)
	usageJSON, err := tensorGridUsageJSON(usage)
	if err != nil {
		return 0, 0, "", "", err
	}
	return deltaMinor, deltaMicroUSD, usageJSON, pricingVersion, nil
}

func TensorGridCreditEventFromConsume(userID int, requestID string, params RecordConsumeLogParams) error {
	var account TensorGridAccount
	if err := DB.Where("user_id = ?", userID).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(requestID) != "" && tensorGridIntegrationConfigured() {
		var settlement TensorGridBillingSettlement
		settlementErr := DB.Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&settlement).Error
		if settlementErr == nil {
			if settlement.Status != TensorGridSettlementSettled {
				return fmt.Errorf("TensorGrid billing request %s is not settled", requestID)
			}
			var outboxCount int64
			if err := DB.Model(&TensorGridCreditOutbox{}).
				Where("event_id = ?", tensorGridOutboxEventID(account.Subject, requestID)).
				Count(&outboxCount).Error; err != nil {
				return err
			}
			if outboxCount != 1 {
				return errors.New("settled TensorGrid billing request is missing its outbox event")
			}
			return nil
		}
		if !errors.Is(settlementErr, gorm.ErrRecordNotFound) {
			return settlementErr
		}
		var adjustment TensorGridBillingAdjustment
		adjustmentErr := DB.Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&adjustment).Error
		if adjustmentErr == nil {
			var outboxCount int64
			if err := DB.Model(&TensorGridCreditOutbox{}).
				Where("event_id = ?", tensorGridOutboxEventID(account.Subject, requestID)).
				Count(&outboxCount).Error; err != nil {
				return err
			}
			if outboxCount != 1 {
				return errors.New("applied TensorGrid billing adjustment is missing its outbox event")
			}
			return nil
		}
		if !errors.Is(adjustmentErr, gorm.ErrRecordNotFound) {
			return adjustmentErr
		}
	}
	deltaMinor, deltaMicroUSD, usageJSON, pricingVersion, err := tensorGridCreditEventValues(&account, params)
	if err != nil {
		return err
	}
	var usage map[string]interface{}
	if err := common.Unmarshal([]byte(usageJSON), &usage); err != nil {
		return err
	}
	return EnqueueTensorGridCreditEvent(account.Subject, requestID, deltaMinor, deltaMicroUSD, usage, pricingVersion)
}

func tensorGridIntegrationConfigured() bool {
	return len(strings.TrimSpace(os.Getenv("TENSORGRID_INTEGRATION_SECRET"))) >= 32
}

// ReserveTensorGridWalletQuota records the request reservation together with
// the wallet deduction. handled is false for non-TensorGrid users, allowing the
// upstream Gateway billing path to retain its existing behavior.
func ReserveTensorGridWalletQuota(userID int, requestID string, targetQuota int) (handled, reserved bool, err error) {
	if !tensorGridIntegrationConfigured() {
		return false, false, nil
	}
	requestID = strings.TrimSpace(requestID)
	if targetQuota < 0 {
		return false, false, errors.New("TensorGrid reservation quota cannot be negative")
	}
	err = DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		accountErr := lockForUpdate(tx).Where("user_id = ?", userID).First(&account).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if accountErr != nil {
			return accountErr
		}
		handled = true
		if requestID == "" {
			return errors.New("TensorGrid reservation requires request id")
		}

		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		var settlement TensorGridBillingSettlement
		settlementErr := lockForUpdate(tx).
			Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&settlement).Error
		if settlementErr == nil {
			if settlement.Status != TensorGridSettlementReserved || targetQuota < settlement.ReservedQuota {
				return ErrTensorGridIdempotencyConflict
			}
			additionalQuota := targetQuota - settlement.ReservedQuota
			if user.Quota < additionalQuota {
				return nil
			}
			if additionalQuota > 0 {
				user.Quota -= additionalQuota
				if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", user.Quota).Error; err != nil {
					return err
				}
				if err := tx.Model(&settlement).Updates(map[string]interface{}{
					"reserved_quota": targetQuota, "balance_quota_after": user.Quota,
					"updated_at": time.Now().UTC(),
				}).Error; err != nil {
					return err
				}
			}
			reserved = true
			return nil
		}
		if !errors.Is(settlementErr, gorm.ErrRecordNotFound) {
			return settlementErr
		}
		if user.Quota < targetQuota {
			return nil
		}
		user.Quota -= targetQuota
		if targetQuota > 0 {
			if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", user.Quota).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if err := tx.Create(&TensorGridBillingSettlement{
			AccountId: account.Id, RequestId: requestID, ReservedQuota: targetQuota,
			BalanceQuotaAfter: user.Quota, Status: TensorGridSettlementReserved,
			ReservedAt: now,
		}).Error; err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err == nil && handled {
		if cacheErr := invalidateUserCache(userID); cacheErr != nil {
			common.SysLog("failed to invalidate TensorGrid wallet cache after reservation: " + cacheErr.Error())
		}
	}
	return handled, reserved, err
}

// SettleTensorGridWalletQuota atomically commits the final wallet delta and
// its outbox event. Repeating the same request is a no-op; changing the charge
// for an already-settled request is rejected as an idempotency conflict.
func SettleTensorGridWalletQuota(userID int, requestID string, actualQuota int, params RecordConsumeLogParams) (handled bool, err error) {
	if !tensorGridIntegrationConfigured() {
		return false, nil
	}
	requestID = strings.TrimSpace(requestID)
	if actualQuota < 0 {
		return false, errors.New("TensorGrid settlement quota cannot be negative")
	}
	params.Quota = actualQuota

	err = DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		accountErr := lockForUpdate(tx).Where("user_id = ?", userID).First(&account).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if accountErr != nil {
			return accountErr
		}
		handled = true
		if requestID == "" {
			return errors.New("TensorGrid settlement requires request id")
		}

		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		var settlement TensorGridBillingSettlement
		if err := lockForUpdate(tx).
			Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&settlement).Error; err != nil {
			return err
		}
		if settlement.Status == TensorGridSettlementSettled {
			if settlement.ActualQuota != actualQuota {
				return ErrTensorGridIdempotencyConflict
			}
			return nil
		}
		if settlement.Status != TensorGridSettlementReserved {
			return ErrTensorGridIdempotencyConflict
		}

		settlementDelta := actualQuota - settlement.ReservedQuota
		collectedQuota := actualQuota
		if settlementDelta > user.Quota {
			collectedQuota = settlement.ReservedQuota + user.Quota
			user.Quota = 0
		} else {
			user.Quota -= settlementDelta
		}
		if settlementDelta != 0 {
			if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", user.Quota).Error; err != nil {
				return err
			}
		}
		eventParams := params
		eventParams.Quota = collectedQuota
		if eventParams.Other == nil {
			eventParams.Other = NewLogOther()
		}
		// LogOther is a pointer, so these land on the caller's log too — the same
		// sharing the map-based version had, and what makes billed vs collected
		// visible in the consume log.
		eventParams.Other.SetPublic("billed_quota", actualQuota)
		eventParams.Other.SetPublic("collected_quota", collectedQuota)
		if uncollectedQuota := actualQuota - collectedQuota; uncollectedQuota > 0 {
			eventParams.Other.SetPublic("uncollected_quota", uncollectedQuota)
		}
		deltaMinor, deltaMicroUSD, usageJSON, pricingVersion, err := tensorGridCreditEventValues(&account, eventParams)
		if err != nil {
			return err
		}
		if err := enqueueTensorGridCreditEventTx(
			tx, &account, user.Quota, requestID, deltaMinor, deltaMicroUSD,
			usageJSON, pricingVersion,
		); err != nil {
			return err
		}
		now := time.Now().UTC()
		return tx.Model(&settlement).Updates(map[string]interface{}{
			"actual_quota": actualQuota, "balance_quota_after": user.Quota,
			"status": TensorGridSettlementSettled, "settled_at": now, "updated_at": now,
		}).Error
	})
	if err == nil && handled {
		if cacheErr := invalidateUserCache(userID); cacheErr != nil {
			common.SysLog("failed to invalidate TensorGrid wallet cache after settlement: " + cacheErr.Error())
		}
	}
	return handled, err
}

// AdjustTensorGridWalletQuota atomically applies a wallet change that is not
// part of the initial request reservation. A positive delta charges quota and
// a negative delta refunds it. Reusing requestID is idempotent.
func AdjustTensorGridWalletQuota(userID int, requestID string, requestedQuotaDelta int, params RecordConsumeLogParams) (handled bool, appliedQuotaDelta int, err error) {
	if !tensorGridIntegrationConfigured() {
		return false, 0, nil
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 {
		return false, 0, errors.New("TensorGrid billing adjustment requires a request id of at most 128 characters")
	}
	if requestedQuotaDelta < -common.MaxQuota || requestedQuotaDelta > common.MaxQuota {
		return false, 0, errors.New("TensorGrid billing adjustment is outside the supported quota range")
	}

	err = DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		accountErr := lockForUpdate(tx).Where("user_id = ?", userID).First(&account).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if accountErr != nil {
			return accountErr
		}
		handled = true

		var existing TensorGridBillingAdjustment
		existingErr := lockForUpdate(tx).
			Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&existing).Error
		if existingErr == nil {
			if existing.RequestedQuotaDelta != requestedQuotaDelta {
				return ErrTensorGridIdempotencyConflict
			}
			appliedQuotaDelta = existing.AppliedQuotaDelta
			return nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}

		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		appliedQuotaDelta = requestedQuotaDelta
		if appliedQuotaDelta > user.Quota {
			appliedQuotaDelta = user.Quota
		}
		balanceAfter := int64(user.Quota) - int64(appliedQuotaDelta)
		if balanceAfter < 0 || balanceAfter > int64(common.MaxWalletQuota) {
			return errors.New("TensorGrid billing adjustment would move the balance outside the supported range")
		}

		// A detached copy, unlike the settlement path above: these adjustment keys
		// describe the credit event alone and must not reach the caller's log.
		// Privileged scopes are dropped on the way, which is what the event wants.
		eventParams := params
		eventParams.Other = NewLogOther()
		eventParams.Other.MergePublic(params.Other.Snapshot())
		eventParams.Other.SetPublic("requested_quota_delta", requestedQuotaDelta)
		magnitude := appliedQuotaDelta
		if magnitude < 0 {
			magnitude = -magnitude
			eventParams.Other.SetPublic("refund_quota", magnitude)
		} else {
			eventParams.Other.SetPublic("billed_quota", requestedQuotaDelta)
			eventParams.Other.SetPublic("collected_quota", appliedQuotaDelta)
			if uncollectedQuota := requestedQuotaDelta - appliedQuotaDelta; uncollectedQuota > 0 {
				eventParams.Other.SetPublic("uncollected_quota", uncollectedQuota)
			}
		}
		eventParams.Quota = magnitude
		deltaMinor, deltaMicroUSD, usageJSON, pricingVersion, err := tensorGridCreditEventValues(&account, eventParams)
		if err != nil {
			return err
		}
		if appliedQuotaDelta < 0 {
			deltaMinor = -deltaMinor
			deltaMicroUSD = -deltaMicroUSD
		}

		if appliedQuotaDelta != 0 {
			if err := tx.Model(&User{}).Where("id = ?", userID).
				Update("quota", int(balanceAfter)).Error; err != nil {
				return err
			}
		}
		if err := enqueueTensorGridCreditEventTx(
			tx, &account, int(balanceAfter), requestID, deltaMinor, deltaMicroUSD,
			usageJSON, pricingVersion,
		); err != nil {
			return err
		}
		return tx.Create(&TensorGridBillingAdjustment{
			AccountId: account.Id, RequestId: requestID,
			RequestedQuotaDelta: requestedQuotaDelta,
			AppliedQuotaDelta:   appliedQuotaDelta, BalanceQuotaAfter: int(balanceAfter),
		}).Error
	})
	if err == nil && handled {
		if cacheErr := invalidateUserCache(userID); cacheErr != nil {
			common.SysLog("failed to invalidate TensorGrid wallet cache after adjustment: " + cacheErr.Error())
		}
	}
	return handled, appliedQuotaDelta, err
}

// RefundTensorGridWalletQuota restores a reservation exactly once. Settled
// requests cannot be refunded through this pre-provider failure path.
func RefundTensorGridWalletQuota(userID int, requestID string) (handled bool, err error) {
	if !tensorGridIntegrationConfigured() {
		return false, nil
	}
	requestID = strings.TrimSpace(requestID)
	err = DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		accountErr := lockForUpdate(tx).Where("user_id = ?", userID).First(&account).Error
		if errors.Is(accountErr, gorm.ErrRecordNotFound) {
			return nil
		}
		if accountErr != nil {
			return accountErr
		}
		handled = true
		if requestID == "" {
			return errors.New("TensorGrid refund requires request id")
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			return err
		}
		var settlement TensorGridBillingSettlement
		if err := lockForUpdate(tx).
			Where("account_id = ? AND request_id = ?", account.Id, requestID).
			First(&settlement).Error; err != nil {
			return err
		}
		if settlement.Status == TensorGridSettlementRefunded {
			return nil
		}
		if settlement.Status != TensorGridSettlementReserved {
			return ErrTensorGridIdempotencyConflict
		}
		user.Quota += settlement.ReservedQuota
		if settlement.ReservedQuota > 0 {
			if err := tx.Model(&User{}).Where("id = ?", userID).Update("quota", user.Quota).Error; err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		if err := tx.Model(&settlement).Updates(map[string]interface{}{
			"balance_quota_after": user.Quota, "status": TensorGridSettlementRefunded,
			"refunded_at": now, "updated_at": now,
		}).Error; err != nil {
			return err
		}
		snapshot, err := snapshotTensorGridBalance(&account, user.Quota)
		if err != nil {
			return err
		}
		eventID := "snapshot:refund:" + account.Subject + ":" + requestID
		var existing TensorGridCreditOutbox
		if err := tx.Where("event_id = ?", eventID).First(&existing).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return tx.Create(&TensorGridCreditOutbox{
			EventId: eventID, Kind: TensorGridOutboxSnapshot, Subject: account.Subject,
			AccountId: account.Id, RequestId: "refund:" + requestID,
			Sequence: account.CreditSequence, Currency: account.Currency,
			BalanceMinor: snapshot.BalanceMinor, BalanceMicroUSD: snapshot.BalanceMicroUSD,
			UsageBreakdown: "{}", OccurredAt: now, NextAttemptAt: now,
		}).Error
	})
	if err == nil && handled {
		if cacheErr := invalidateUserCache(userID); cacheErr != nil {
			common.SysLog("failed to invalidate TensorGrid wallet cache after refund: " + cacheErr.Error())
		}
	}
	return handled, err
}

// RefundStaleTensorGridWalletReservations recovers wallet reservations left by
// a process crash before settlement or the normal request-failure refund path.
func RefundStaleTensorGridWalletReservations(before time.Time, limit int) (refunded, failed int, err error) {
	if !tensorGridIntegrationConfigured() {
		return 0, 0, nil
	}
	if limit < 1 {
		limit = 100
	}
	var settlements []TensorGridBillingSettlement
	if err := DB.Where("status = ? AND reserved_at <= ?", TensorGridSettlementReserved, before.UTC()).
		Order("reserved_at asc, id asc").Limit(limit).Find(&settlements).Error; err != nil {
		return 0, 0, err
	}
	for _, settlement := range settlements {
		var account TensorGridAccount
		if err := DB.Select("user_id").Where("id = ?", settlement.AccountId).First(&account).Error; err != nil {
			failed++
			continue
		}
		handled, refundErr := RefundTensorGridWalletQuota(account.UserId, settlement.RequestId)
		if errors.Is(refundErr, ErrTensorGridIdempotencyConflict) {
			continue
		}
		if refundErr != nil || !handled {
			failed++
			continue
		}
		refunded++
	}
	return refunded, failed, nil
}

// ReconcileTensorGridCreditEvents rebuilds missing outbox rows from durable
// consume logs. Settlement and log persistence are separate tables in the
// upstream application, so this bounded replay closes the crash window between
// them while the outbox's event id keeps the business effect idempotent.
func ReconcileTensorGridCreditEvents(limit int) (replayed, failed int, err error) {
	tensorGridReconcileMu.Lock()
	defer tensorGridReconcileMu.Unlock()
	if limit < 1 {
		limit = 1000
	}
	if LOG_DB == nil {
		return 0, 0, errors.New("log database is not initialized")
	}
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	var logs []*Log
	if err := LOG_DB.Where("type = ? AND created_at >= ?", LogTypeConsume, cutoff).
		Order("created_at asc, id asc").Offset(tensorGridReconcileOffset).
		Limit(limit).Find(&logs).Error; err != nil {
		return 0, 0, err
	}
	if len(logs) < limit {
		tensorGridReconcileOffset = 0
	} else {
		tensorGridReconcileOffset += len(logs)
	}
	for _, log := range logs {
		parsed, parseErr := common.StrToMap(log.Other)
		if parseErr != nil {
			failed++
			continue
		}
		// Rebuild the stored log's top level as the public scope: every usage key
		// the credit event reads was written there, and the privileged scopes
		// SetPublic rejects are not part of the event payload.
		other := NewLogOther()
		other.MergePublic(parsed)
		requestID := log.RequestId
		if strings.TrimSpace(requestID) == "" {
			requestID = fmt.Sprintf("log:%d", log.Id)
		}
		params := RecordConsumeLogParams{
			ChannelId: log.ChannelId, PromptTokens: log.PromptTokens,
			CompletionTokens: log.CompletionTokens, ModelName: log.ModelName,
			TokenName: log.TokenName, Quota: log.Quota, Content: log.Content,
			TokenId: log.TokenId, UseTimeSeconds: log.UseTime, IsStream: log.IsStream,
			Group: log.Group, Other: other,
		}
		if err := TensorGridCreditEventFromConsume(log.UserId, requestID, params); err != nil {
			failed++
			continue
		}
		replayed++
	}
	return replayed, failed, nil
}

func tensorGridLogString(other map[string]interface{}, key string) string {
	if value, ok := other[key].(string); ok {
		return value
	}
	return ""
}

func tensorGridFirstLogNumber(other map[string]interface{}, keys ...string) int64 {
	for _, key := range keys {
		if value := tensorGridLogNumber(other, key); value != 0 {
			return value
		}
	}
	return 0
}

func tensorGridPricingVersion(modelName string, other map[string]interface{}) string {
	version := map[string]interface{}{"model": modelName}
	for _, key := range []string{
		"billing_mode", "expr_b64", "model_price", "model_ratio", "group_ratio",
		"user_group_ratio", "completion_ratio", "cache_ratio", "cache_creation_ratio",
		"cache_creation_ratio_5m", "cache_creation_ratio_1h", "image_ratio",
		"audio_ratio", "audio_completion_ratio", "audio_input_price",
	} {
		if value, ok := other[key]; ok {
			version[key] = value
		}
	}
	payload, err := common.Marshal(version)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func tensorGridOutboxEventID(subject, requestID string) string {
	if strings.TrimSpace(requestID) == "" {
		return "event:" + uuid.NewString()
	}
	return "event:" + subject + ":" + strings.TrimSpace(requestID)
}

func tensorGridUsageJSON(usage map[string]interface{}) (string, error) {
	if usage == nil {
		usage = map[string]interface{}{}
	}
	payload, err := common.Marshal(usage)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func enqueueTensorGridCreditEventTx(
	tx *gorm.DB, account *TensorGridAccount, balanceQuota int, requestID string,
	deltaMinor, deltaMicroUSD int64, usageJSON, pricingVersion string,
) error {
	eventID := tensorGridOutboxEventID(account.Subject, requestID)
	var existing TensorGridCreditOutbox
	existingErr := tx.Where("event_id = ?", eventID).First(&existing).Error
	if existingErr == nil {
		if existing.Subject != account.Subject || existing.Currency != account.Currency ||
			existing.DeltaMinor != deltaMinor || existing.DeltaMicroUSD != deltaMicroUSD {
			return ErrTensorGridIdempotencyConflict
		}
		return nil
	}
	if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
		return existingErr
	}
	account.CreditSequence++
	if err := tx.Model(account).Update("credit_sequence", account.CreditSequence).Error; err != nil {
		return err
	}
	snapshot, err := snapshotTensorGridBalance(account, balanceQuota)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	row := TensorGridCreditOutbox{
		EventId: eventID, Kind: TensorGridOutboxEvent, Subject: account.Subject,
		AccountId: account.Id, RequestId: requestID, Sequence: account.CreditSequence,
		Currency: account.Currency, DeltaMinor: deltaMinor, DeltaMicroUSD: deltaMicroUSD,
		BalanceMinor: snapshot.BalanceMinor, BalanceMicroUSD: snapshot.BalanceMicroUSD,
		UsageBreakdown: usageJSON, PricingVersion: pricingVersion,
		OccurredAt: now, NextAttemptAt: now,
	}
	return tx.Create(&row).Error
}

// EnqueueTensorGridCreditEvent serializes a settled Gateway mutation with the
// account lock. The event is the only durable writer-facing hand-off to
// TensorGrid; delivery itself is deliberately retriable and asynchronous.
func EnqueueTensorGridCreditEvent(
	subject, requestID string, deltaMinor, deltaMicroUSD int64,
	usage map[string]interface{}, pricingVersion string,
) error {
	normalized, err := normalizeTensorGridSubject(subject)
	if err != nil {
		return err
	}
	usageJSON, err := tensorGridUsageJSON(usage)
	if err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		if err := lockForUpdate(tx).Where("subject = ?", normalized).First(&account).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTensorGridAccountNotFound
			}
			return err
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", account.UserId).First(&user).Error; err != nil {
			return err
		}
		return enqueueTensorGridCreditEventTx(
			tx, &account, user.Quota, requestID, deltaMinor, deltaMicroUSD,
			usageJSON, pricingVersion,
		)
	})
}

// EnqueueTensorGridBalanceSnapshot creates a stable snapshot event for the
// current account sequence. Re-running reconciliation therefore converges on a
// single event instead of producing an unbounded stream of no-op rows.
func EnqueueTensorGridBalanceSnapshot(subject string) error {
	normalized, err := normalizeTensorGridSubject(subject)
	if err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var account TensorGridAccount
		if err := lockForUpdate(tx).Where("subject = ?", normalized).First(&account).Error; err != nil {
			return err
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", account.UserId).First(&user).Error; err != nil {
			return err
		}
		snapshot, err := snapshotTensorGridBalance(&account, user.Quota)
		if err != nil {
			return err
		}
		eventID := fmt.Sprintf("snapshot:%s:%d:%d:%d", normalized, account.CreditSequence, snapshot.BalanceMinor, snapshot.BalanceMicroUSD)
		var existing TensorGridCreditOutbox
		if err := tx.Where("event_id = ?", eventID).First(&existing).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now().UTC()
		return tx.Create(&TensorGridCreditOutbox{
			EventId: eventID, Kind: TensorGridOutboxSnapshot, Subject: normalized,
			AccountId: account.Id, Sequence: account.CreditSequence, Currency: account.Currency,
			BalanceMinor: snapshot.BalanceMinor, BalanceMicroUSD: snapshot.BalanceMicroUSD,
			UsageBreakdown: "{}", OccurredAt: now, NextAttemptAt: now,
		}).Error
	})
}

func GetTensorGridCreditOutboxBatch(limit int) ([]*TensorGridCreditOutbox, error) {
	if limit < 1 {
		limit = 100
	}
	rows := make([]*TensorGridCreditOutbox, 0, limit)
	err := DB.Where("delivered_at IS NULL AND next_attempt_at <= ?", time.Now().UTC()).
		Order("next_attempt_at asc, id asc").Limit(limit).Find(&rows).Error
	return rows, err
}

func ListTensorGridSubjectsAfter(afterID int64, limit int) ([]TensorGridAccount, error) {
	if limit < 1 {
		limit = 500
	}
	var accounts []TensorGridAccount
	if err := DB.Select("id", "subject").Where("id > ?", afterID).
		Order("id asc").Limit(limit).Find(&accounts).Error; err != nil {
		return nil, err
	}
	return accounts, nil
}

func MarkTensorGridCreditOutboxDelivered(id int64) error {
	now := time.Now().UTC()
	return DB.Model(&TensorGridCreditOutbox{}).Where("id = ?", id).Updates(map[string]interface{}{
		"delivered_at": now, "updated_at": now, "last_error": "",
	}).Error
}

func MarkTensorGridCreditOutboxFailed(id int64, deliveryErr error) error {
	row := TensorGridCreditOutbox{}
	if err := DB.First(&row, id).Error; err != nil {
		return err
	}
	row.Attempts++
	backoff := time.Second * time.Duration(1<<minInt(row.Attempts, 8))
	if backoff > 15*time.Minute {
		backoff = 15 * time.Minute
	}
	row.NextAttemptAt = time.Now().UTC().Add(backoff)
	row.LastError = deliveryErr.Error()
	return DB.Model(&row).Updates(map[string]interface{}{
		"attempts": row.Attempts, "next_attempt_at": row.NextAttemptAt,
		"last_error": row.LastError, "updated_at": time.Now().UTC(),
	}).Error
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
