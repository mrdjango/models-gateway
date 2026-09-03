package model

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTensorGridModelTest(t *testing.T) {
	t.Helper()
	t.Setenv("TENSORGRID_INTEGRATION_SECRET", "tensorgrid-model-test-secret-at-least-32-bytes")
	previousDB := DB
	previousLogDB := LOG_DB
	previousRedis := common.RedisEnabled
	previousBatch := common.BatchUpdateEnabled
	previousQuotaPerUnit := common.QuotaPerUnit
	previousOptionMap := common.OptionMap
	previousBillingModes := billing_setting.GetBillingModeCopy()
	previousBillingExpressions := billing_setting.GetBillingExprCopy()
	previousModelRatios := ratio_setting.GetModelRatioCopy()
	previousModelPrices := ratio_setting.GetModelPriceCopy()
	previousCompletionRatios := ratio_setting.GetCompletionRatioCopy()
	previousCacheRatios := ratio_setting.GetCacheRatioCopy()
	previousCreateCacheRatios := ratio_setting.GetCreateCacheRatioCopy()

	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	DB = db
	LOG_DB = db
	common.RedisEnabled = false
	common.BatchUpdateEnabled = false
	common.QuotaPerUnit = 500_000
	common.OptionMapRWMutex.Lock()
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	require.NoError(t, DB.AutoMigrate(
		&User{}, &Token{}, &Channel{}, &Ability{}, &Option{}, &Log{}, &TensorGridAccount{},
		&TensorGridBalanceMutation{}, &TensorGridTokenCreation{}, &TensorGridCreditOutbox{},
		&TensorGridBillingSettlement{}, &TensorGridBillingAdjustment{},
		&Model{}, &Vendor{},
	))

	t.Cleanup(func() {
		if modes, marshalErr := common.Marshal(previousBillingModes); marshalErr == nil {
			_ = UpdateOption("billing_setting.billing_mode", string(modes))
		}
		if expressions, marshalErr := common.Marshal(previousBillingExpressions); marshalErr == nil {
			_ = UpdateOption("billing_setting.billing_expr", string(expressions))
		}
		for key, value := range map[string]interface{}{
			"ModelRatio": previousModelRatios, "ModelPrice": previousModelPrices,
			"CompletionRatio": previousCompletionRatios, "CacheRatio": previousCacheRatios,
			"CreateCacheRatio": previousCreateCacheRatios,
		} {
			if encoded, marshalErr := common.Marshal(value); marshalErr == nil {
				_ = UpdateOption(key, string(encoded))
			}
		}
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		DB = previousDB
		LOG_DB = previousLogDB
		common.RedisEnabled = previousRedis
		common.BatchUpdateEnabled = previousBatch
		common.QuotaPerUnit = previousQuotaPerUnit
	})
}

// publicLogOther builds the log metadata a consume log carries, with every key
// in the public scope — where the TensorGrid usage meters read them from.
func publicLogOther(values map[string]interface{}) *LogOther {
	other := NewLogOther()
	other.MergePublic(values)
	return other
}

func TestTensorGridBalanceAdjustmentIsIdempotent(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "b359938a-1b18-4a0b-8184-c99990a9ce74"

	account, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "USD", "", true, 1)
	require.NoError(t, err)
	assert.Equal(t, TensorGridCurrencyUSD, account.Currency)

	first, created, _, _, err := AdjustTensorGridBalance(subject, "payment:00000001", "USD", 100, 0, "purchase", false)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(100), first.BalanceMinor)
	assert.Equal(t, 500_000, first.BalanceQuota)

	replayed, created, _, _, err := AdjustTensorGridBalance(subject, "payment:00000001", "USD", 100, 0, "purchase", false)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.BalanceQuota, replayed.BalanceQuota)
	var paymentOutbox []TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "adjust:payment:00000001").Find(&paymentOutbox).Error)
	assert.Len(t, paymentOutbox, 1)
	assert.Equal(t, int64(100), paymentOutbox[0].DeltaMinor)
	assert.Equal(t, first.BalanceMinor, paymentOutbox[0].BalanceMinor)

	_, _, _, _, err = AdjustTensorGridBalance(subject, "payment:00000001", "USD", 200, 0, "conflict", false)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)

	debited, created, _, _, err := AdjustTensorGridBalance(subject, "compute:00000001", "USD", -25, 0, "compute usage", false)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(75), debited.BalanceMinor)
	assert.Equal(t, 375_000, debited.BalanceQuota)
}

func TestTensorGridBalanceAdjustmentStoresFullLengthIdempotencyKeyInOutbox(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "b1c2d3e4-f5a6-4788-9a0b-1c2d3e4f5a6b"

	account, err := UpsertTensorGridAccount(subject, "long@example.com", "Long", "USD", "", true, 1)
	require.NoError(t, err)

	// The backend accepts idempotency keys up to 128 characters. The gateway then
	// composes "event:<subject>:adjust:<key>", which must persist without truncation.
	idempotencyKey := "hold:" + strings.Repeat("a", 128-len("hold:"))
	require.Len(t, idempotencyKey, 128)

	_, created, _, _, err := AdjustTensorGridBalance(subject, idempotencyKey, "USD", 100, 0, "long key", false)
	require.NoError(t, err)
	assert.True(t, created)

	var outbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("account_id = ?", account.Id).First(&outbox).Error)
	assert.Equal(t, "adjust:"+idempotencyKey, outbox.RequestId)
	assert.Equal(t, "event:"+subject+":adjust:"+idempotencyKey, outbox.EventId)
	assert.LessOrEqual(t, len(outbox.EventId), 256)
}

func TestTensorGridBalanceRejectsCurrencyMismatchAndOverdraft(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "32d16ba6-8a67-45fa-bf4b-1a4574e332fc"

	_, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "IRT", "1000000", true, 1)
	require.NoError(t, err)

	_, _, _, _, err = AdjustTensorGridBalance(subject, "payment:00000002", "USD", 1_000_000, 0, "wrong currency", false)
	assert.ErrorIs(t, err, ErrTensorGridCurrencyMismatch)

	_, _, _, _, err = AdjustTensorGridBalance(subject, "compute:00000002", "IRT", -1, 0, "overdraft", false)
	assert.ErrorContains(t, err, "insufficient gateway balance")
}

func TestTensorGridAccountRejectsOutOfRangeFxRate(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "6f3a1d8e-2b4c-4a1e-9f0d-7c8b5a2e1f30"

	// decimal renders "1e1000" as a 1001-char fixed-point string, which would
	// overflow fx_rate_irt_per_usd varchar(64) and fail the write with 22001.
	_, err := UpsertTensorGridAccount(subject, "fx@example.com", "Fx", "IRT", "1e1000", true, 1)
	assert.ErrorContains(t, err, "out of range")

	_, err = UpsertTensorGridAccount(subject, "fx@example.com", "Fx", "IRT", "197150.712115", true, 1)
	require.NoError(t, err)
}

func TestTensorGridBalanceAdjustmentTruncatesReasonOnRuneBoundary(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "7a4b2c9d-3e5f-4b2a-8c1d-9e0f6a3b2c41"

	account, err := UpsertTensorGridAccount(subject, "reason@example.com", "Reason", "USD", "", true, 1)
	require.NoError(t, err)

	// 300 multi-byte runes: a byte slice at [:255] would split one and Postgres
	// would reject the invalid UTF-8. Rune truncation keeps it valid.
	longReason := strings.Repeat("é", 300)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "adjust:rune-reason", "USD", 100, 0, longReason, false)
	require.NoError(t, err)

	var mutation TensorGridBalanceMutation
	require.NoError(t, DB.Where("account_id = ?", account.Id).First(&mutation).Error)
	assert.Equal(t, 255, len([]rune(mutation.Reason)))
	assert.True(t, utf8.ValidString(mutation.Reason))
}

func TestTensorGridBalanceRequiresImmediateDatabaseUpdates(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "59054933-ed63-477f-aaed-c1e9c690ec1c"

	_, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "USD", "", true, 1)
	require.NoError(t, err)
	common.BatchUpdateEnabled = true

	_, _, _, _, err = AdjustTensorGridBalance(subject, "payment:00000003", "USD", 100, 0, "purchase", false)
	assert.ErrorContains(t, err, "BATCH_UPDATE_ENABLED=false")
}

func TestTensorGridIrtRateRefreshPreservesNominalBalance(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "749ea2a7-7cb8-49ed-b9df-e411bc746180"

	_, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "IRT", "1000000", true, 1)
	require.NoError(t, err)
	before, created, _, _, err := AdjustTensorGridBalance(subject, "payment:00000004", "IRT", 1_000_000, 0, "purchase", false)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(1_000_000), before.BalanceMinor)

	_, err = UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "IRT", "2000000", true, 2)
	require.NoError(t, err)
	after, err := GetTensorGridBalance(subject)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000_000), after.BalanceMinor)
	assert.Equal(t, before.BalanceQuota/2, after.BalanceQuota)
}

func TestTensorGridUserSyncIgnoresStaleSecurityState(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "d80e9f3c-0731-48db-b8bb-350f20aa3ebf"

	account, err := UpsertTensorGridAccount(
		subject, "owner@example.com", "Owner", "USD", "", true, 2,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(2), account.SyncVersion)

	account, err = UpsertTensorGridAccount(
		subject, "stale@example.com", "Stale", "USD", "", false, 1,
	)
	require.NoError(t, err)
	assert.Equal(t, int64(2), account.SyncVersion)

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, common.UserStatusEnabled, user.Status)
	assert.Equal(t, "owner@example.com", user.Email)
}

func TestTensorGridPartialDebitCollectsAvailableBalanceExactlyOnce(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "1a59288f-d387-42d8-bb9b-a242054c3870"

	_, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "payment:00000005", "USD", 100, 0, "purchase", false)
	require.NoError(t, err)

	snapshot, created, appliedMinor, appliedMicroUSD, err := AdjustTensorGridBalance(
		subject, "provider:00000001", "USD", 0, -2_000_000, "provider usage", true,
	)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(0), snapshot.BalanceMinor)
	assert.Equal(t, int64(-100), appliedMinor)
	assert.Equal(t, int64(-1_000_000), appliedMicroUSD)

	replayed, created, replayedMinor, replayedMicroUSD, err := AdjustTensorGridBalance(
		subject, "provider:00000001", "USD", 0, -2_000_000, "provider usage", true,
	)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, snapshot.BalanceQuota, replayed.BalanceQuota)
	assert.Equal(t, appliedMinor, replayedMinor)
	assert.Equal(t, appliedMicroUSD, replayedMicroUSD)
}

func TestTensorGridTokenCreationIsAccountScopedAndIdempotent(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "80435416-68c3-489d-b36b-fbda56c2dde9"

	_, err := UpsertTensorGridAccount(subject, "owner@example.com", "Owner", "USD", "", true, 1)
	require.NoError(t, err)
	first, created, err := CreateTensorGridToken(subject, "token:00000001", "Production", 0)
	require.NoError(t, err)
	assert.True(t, created)
	assert.NotEmpty(t, first.GetFullKey())

	replayed, created, err := CreateTensorGridToken(subject, "token:00000001", "Production", 0)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.Id, replayed.Id)
	assert.Equal(t, first.GetFullKey(), replayed.GetFullKey())
	revealedAgain, err := GetTokenByIds(first.Id, first.UserId)
	require.NoError(t, err)
	assert.Equal(t, first.GetFullKey(), revealedAgain.GetFullKey())

	_, _, err = CreateTensorGridToken(subject, "token:00000001", "Different", 0)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)
}

func TestTensorGridCreditEventReplayIsIdempotent(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "d6ec9d8b-48fa-4931-a410-2d6426324014"
	account, err := UpsertTensorGridAccount(subject, "usage@example.com", "Usage", "USD", "", true, 1)
	require.NoError(t, err)
	require.NoError(t, LOG_DB.Create(&Log{
		UserId: account.UserId, Type: LogTypeConsume, CreatedAt: time.Now().Unix(),
		Quota: 500, PromptTokens: 10, CompletionTokens: 20, RequestId: "replay-request",
		Other: common.MapToJsonStr(map[string]interface{}{"cache_tokens": 3}),
	}).Error)

	replayed, failed, err := ReconcileTensorGridCreditEvents(100)
	require.NoError(t, err)
	assert.Equal(t, 1, replayed)
	assert.Zero(t, failed)

	replayed, failed, err = ReconcileTensorGridCreditEvents(100)
	require.NoError(t, err)
	assert.Equal(t, 1, replayed)
	assert.Zero(t, failed)
	var outbox []TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "replay-request").Find(&outbox).Error)
	assert.Len(t, outbox, 1)
}

func TestTensorGridCreditEventIncludesEveryUsageMeterAndPricingVersion(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "f0688251-267e-4803-9810-616b0530ad54"
	account, err := UpsertTensorGridAccount(subject, "meters@example.com", "Meters", "USD", "", true, 1)
	require.NoError(t, err)

	err = TensorGridCreditEventFromConsume(account.UserId, "meter-request", RecordConsumeLogParams{
		ModelName: "tiered:model", Quota: 500, PromptTokens: 10, CompletionTokens: 20,
		Other: publicLogOther(map[string]interface{}{
			"cache_tokens": 3, "cache_write_tokens": 4,
			"image_input_tokens": 5, "image_output_tokens": 6,
			"audio_input_tokens": 7, "audio_output_tokens": 8,
			"billing_mode": "tiered_expr", "expr_b64": "ZXhwcg==", "matched_tier": "base",
		}),
	})
	require.NoError(t, err)

	var outbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "meter-request").First(&outbox).Error)
	assert.Len(t, outbox.PricingVersion, 64)
	delivery, err := TensorGridCreditDeliveryFromOutbox(&outbox)
	require.NoError(t, err)
	assert.Equal(t, float64(3), delivery.UsageBreakdown["cache_read_tokens"])
	assert.Equal(t, float64(4), delivery.UsageBreakdown["cache_write_tokens"])
	assert.Equal(t, float64(5), delivery.UsageBreakdown["image_input_tokens"])
	assert.Equal(t, float64(6), delivery.UsageBreakdown["image_output_tokens"])
	assert.Equal(t, float64(7), delivery.UsageBreakdown["audio_input_tokens"])
	assert.Equal(t, float64(8), delivery.UsageBreakdown["audio_output_tokens"])
	assert.Equal(t, "base", delivery.UsageBreakdown["matched_tier"])
}

func TestTensorGridWalletSettlementCommitsBalanceAndOutboxExactlyOnce(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "f8dc5701-a3e1-41cc-a18f-50e95f0a0e3d"
	const requestID = "atomic-settlement-request"

	account, err := UpsertTensorGridAccount(subject, "atomic@example.com", "Atomic", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:atomic", "USD", 200, 0, "seed", false)
	require.NoError(t, err)

	handled, reserved, err := ReserveTensorGridWalletQuota(account.UserId, requestID, 300_000)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.True(t, reserved)

	params := RecordConsumeLogParams{
		ModelName: "atomic-model", PromptTokens: 11, CompletionTokens: 7,
		Other: publicLogOther(map[string]interface{}{"cache_tokens": 3, "cache_write_tokens": 2}),
	}
	handled, err = SettleTensorGridWalletQuota(account.UserId, requestID, 250_000, params)
	require.NoError(t, err)
	assert.True(t, handled)

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 750_000, user.Quota)
	var settlement TensorGridBillingSettlement
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&settlement).Error)
	assert.Equal(t, TensorGridSettlementSettled, settlement.Status)
	assert.Equal(t, 250_000, settlement.ActualQuota)
	assert.Equal(t, 750_000, settlement.BalanceQuotaAfter)

	var outbox []TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", requestID).Find(&outbox).Error)
	require.Len(t, outbox, 1)
	assert.Equal(t, int64(-500_000), outbox[0].DeltaMicroUSD)
	assert.Equal(t, int64(1_500_000), outbox[0].BalanceMicroUSD)

	handled, err = SettleTensorGridWalletQuota(account.UserId, requestID, 250_000, params)
	require.NoError(t, err)
	assert.True(t, handled)
	user, err = GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 750_000, user.Quota)
	require.NoError(t, DB.Where("request_id = ?", requestID).Find(&outbox).Error)
	assert.Len(t, outbox, 1)
}

func TestTensorGridWalletSettlementRollsBackWhenOutboxInsertFails(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "11227b90-5930-4491-99e0-22c8c3c373c1"
	const requestID = "rollback-settlement-request"

	account, err := UpsertTensorGridAccount(subject, "rollback@example.com", "Rollback", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:rollback", "USD", 200, 0, "seed", false)
	require.NoError(t, err)
	_, reserved, err := ReserveTensorGridWalletQuota(account.UserId, requestID, 300_000)
	require.NoError(t, err)
	require.True(t, reserved)

	callbackName := "test:fail_tensorgrid_outbox_insert"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == (TensorGridCreditOutbox{}).TableName() {
			tx.AddError(errors.New("forced TensorGrid outbox failure"))
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Create().Remove(callbackName)
	})

	handled, err := SettleTensorGridWalletQuota(account.UserId, requestID, 250_000, RecordConsumeLogParams{
		ModelName: "rollback-model",
	})
	assert.True(t, handled)
	assert.ErrorContains(t, err, "forced TensorGrid outbox failure")

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 700_000, user.Quota)
	var settlement TensorGridBillingSettlement
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&settlement).Error)
	assert.Equal(t, TensorGridSettlementReserved, settlement.Status)
	assert.Zero(t, settlement.ActualQuota)
	eventErr := TensorGridCreditEventFromConsume(account.UserId, requestID, RecordConsumeLogParams{
		ModelName: "rollback-model", Quota: 250_000,
	})
	assert.ErrorContains(t, eventErr, "is not settled")
	var outboxCount int64
	require.NoError(t, DB.Model(&TensorGridCreditOutbox{}).Where("request_id = ?", requestID).Count(&outboxCount).Error)
	assert.Zero(t, outboxCount)
}

func TestTensorGridWalletSettlementNeverMakesBalanceNegative(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "f66f538b-1e60-4d3b-a080-7163e0430543"
	const requestID = "overrun-settlement-request"

	account, err := UpsertTensorGridAccount(subject, "overrun@example.com", "Overrun", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:overrun", "USD", 200, 0, "seed", false)
	require.NoError(t, err)
	_, reserved, err := ReserveTensorGridWalletQuota(account.UserId, requestID, 900_000)
	require.NoError(t, err)
	require.True(t, reserved)

	handled, err := SettleTensorGridWalletQuota(account.UserId, requestID, 1_200_000, RecordConsumeLogParams{
		ModelName: "overrun-model",
	})
	require.NoError(t, err)
	assert.True(t, handled)
	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Zero(t, user.Quota)

	var outbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&outbox).Error)
	assert.Equal(t, int64(-2_000_000), outbox.DeltaMicroUSD)
	assert.Zero(t, outbox.BalanceMicroUSD)
	delivery, err := TensorGridCreditDeliveryFromOutbox(&outbox)
	require.NoError(t, err)
	assert.Equal(t, float64(1_200_000), delivery.UsageBreakdown["billed_quota"])
	assert.Equal(t, float64(1_000_000), delivery.UsageBreakdown["collected_quota"])
	assert.Equal(t, float64(200_000), delivery.UsageBreakdown["uncollected_quota"])
}

func TestTensorGridWalletAdjustmentIsAtomicIdempotentAndNeverNegative(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "ee91ed9f-a78a-4c6a-986b-397b414ebf4f"
	const chargeRequestID = "task:42:recalculate:0:1200000"

	account, err := UpsertTensorGridAccount(subject, "adjustment@example.com", "Adjustment", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:adjustment", "USD", 200, 0, "seed", false)
	require.NoError(t, err)

	handled, applied, err := AdjustTensorGridWalletQuota(
		account.UserId, chargeRequestID, 1_200_000,
		RecordConsumeLogParams{ModelName: "task-model", Other: publicLogOther(map[string]interface{}{"cache_tokens": 11})},
	)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 1_000_000, applied)
	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Zero(t, user.Quota)

	var chargeOutbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", chargeRequestID).First(&chargeOutbox).Error)
	assert.Equal(t, int64(-2_000_000), chargeOutbox.DeltaMicroUSD)
	delivery, err := TensorGridCreditDeliveryFromOutbox(&chargeOutbox)
	require.NoError(t, err)
	assert.Equal(t, float64(1_200_000), delivery.UsageBreakdown["billed_quota"])
	assert.Equal(t, float64(1_000_000), delivery.UsageBreakdown["collected_quota"])
	assert.Equal(t, float64(200_000), delivery.UsageBreakdown["uncollected_quota"])
	assert.Equal(t, float64(11), delivery.UsageBreakdown["cache_read_tokens"])

	handled, applied, err = AdjustTensorGridWalletQuota(
		account.UserId, chargeRequestID, 1_200_000, RecordConsumeLogParams{ModelName: "task-model"},
	)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 1_000_000, applied)
	require.NoError(t, TensorGridCreditEventFromConsume(
		account.UserId, chargeRequestID, RecordConsumeLogParams{ModelName: "task-model", Quota: 1_200_000},
	))
	var chargeCount int64
	require.NoError(t, DB.Model(&TensorGridCreditOutbox{}).Where("request_id = ?", chargeRequestID).Count(&chargeCount).Error)
	assert.Equal(t, int64(1), chargeCount)

	_, _, err = AdjustTensorGridWalletQuota(
		account.UserId, chargeRequestID, 1_100_000, RecordConsumeLogParams{},
	)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)

	const refundRequestID = "task:42:refund"
	handled, applied, err = AdjustTensorGridWalletQuota(
		account.UserId, refundRequestID, -250_000, RecordConsumeLogParams{ModelName: "task-model"},
	)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, -250_000, applied)
	user, err = GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 250_000, user.Quota)
	var refundOutbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", refundRequestID).First(&refundOutbox).Error)
	assert.Equal(t, int64(500_000), refundOutbox.DeltaMicroUSD)
}

func TestTensorGridWalletAdjustmentRollsBackWhenOutboxInsertFails(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "b68a34a0-034a-4908-afae-d8125307fb24"
	const requestID = "violation-request:adjustment"

	account, err := UpsertTensorGridAccount(subject, "adjustment-rollback@example.com", "Rollback", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:adjustment-rollback", "USD", 200, 0, "seed", false)
	require.NoError(t, err)

	callbackName := "test:fail_tensorgrid_adjustment_outbox_insert"
	require.NoError(t, DB.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == (TensorGridCreditOutbox{}).TableName() {
			tx.AddError(errors.New("forced TensorGrid adjustment outbox failure"))
		}
	}))
	t.Cleanup(func() {
		_ = DB.Callback().Create().Remove(callbackName)
	})

	handled, _, err := AdjustTensorGridWalletQuota(
		account.UserId, requestID, 250_000, RecordConsumeLogParams{ModelName: "violation-model"},
	)
	assert.True(t, handled)
	assert.ErrorContains(t, err, "forced TensorGrid adjustment outbox failure")
	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 1_000_000, user.Quota)
	var adjustmentCount int64
	require.NoError(t, DB.Model(&TensorGridBillingAdjustment{}).Where("request_id = ?", requestID).Count(&adjustmentCount).Error)
	assert.Zero(t, adjustmentCount)
}

func TestTensorGridWalletReservationRefundIsIdempotent(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "a8f17b7c-bc32-4a62-bbb7-40f061c1308e"
	const requestID = "refund-reservation-request"

	account, err := UpsertTensorGridAccount(subject, "refund@example.com", "Refund", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:refund", "USD", 200, 0, "seed", false)
	require.NoError(t, err)
	_, reserved, err := ReserveTensorGridWalletQuota(account.UserId, requestID, 300_000)
	require.NoError(t, err)
	require.True(t, reserved)

	handled, err := RefundTensorGridWalletQuota(account.UserId, requestID)
	require.NoError(t, err)
	assert.True(t, handled)
	handled, err = RefundTensorGridWalletQuota(account.UserId, requestID)
	require.NoError(t, err)
	assert.True(t, handled)

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 1_000_000, user.Quota)
	var settlement TensorGridBillingSettlement
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&settlement).Error)
	assert.Equal(t, TensorGridSettlementRefunded, settlement.Status)
	var snapshots []TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "refund:"+requestID).Find(&snapshots).Error)
	require.Len(t, snapshots, 1)
	assert.Equal(t, TensorGridOutboxSnapshot, snapshots[0].Kind)
	assert.Equal(t, int64(2_000_000), snapshots[0].BalanceMicroUSD)
}

func TestStaleTensorGridWalletReservationRecoveryRefundsOnce(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "fc8e9dfa-33ce-4053-a1d2-6c09c838255f"
	const requestID = "stale-reservation-request"

	account, err := UpsertTensorGridAccount(subject, "stale@example.com", "Stale", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:stale", "USD", 200, 0, "seed", false)
	require.NoError(t, err)
	_, reserved, err := ReserveTensorGridWalletQuota(account.UserId, requestID, 300_000)
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, DB.Model(&TensorGridBillingSettlement{}).
		Where("request_id = ?", requestID).Update("reserved_at", time.Now().UTC().Add(-time.Hour)).Error)

	refunded, failed, err := RefundStaleTensorGridWalletReservations(time.Now().Add(-30*time.Minute), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, refunded)
	assert.Zero(t, failed)
	refunded, failed, err = RefundStaleTensorGridWalletReservations(time.Now(), 10)
	require.NoError(t, err)
	assert.Zero(t, refunded)
	assert.Zero(t, failed)

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 1_000_000, user.Quota)
}

func TestListTensorGridSubjectsAfterPagesEveryAccount(t *testing.T) {
	setupTensorGridModelTest(t)
	subjects := []string{
		"5d765485-53fe-4df2-8930-74de4f9c8211",
		"c68fcd8f-24c9-4480-854a-3fdb03f5e298",
		"8fca1e56-9e0e-4457-87ef-baa5207053c5",
	}
	for index, subject := range subjects {
		_, err := UpsertTensorGridAccount(
			subject, fmt.Sprintf("page-%d@example.com", index), "Page", "USD", "", true, int64(index+1),
		)
		require.NoError(t, err)
	}

	first, err := ListTensorGridSubjectsAfter(0, 2)
	require.NoError(t, err)
	require.Len(t, first, 2)
	second, err := ListTensorGridSubjectsAfter(first[1].Id, 2)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, subjects[2], second[0].Subject)
}

func TestTensorGridUsageTotalsCoverAllFilteredRowsNotOnlyThePage(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "5cbdf91a-104d-4f2f-938d-b235a3e51f01"
	account, err := UpsertTensorGridAccount(subject, "usage@example.com", "Usage", "USD", "", true, 1)
	require.NoError(t, err)
	logs := []Log{
		{
			UserId: account.UserId, Type: LogTypeConsume, CreatedAt: 100, ModelName: "model-a",
			TokenName: "Production", Quota: 500, PromptTokens: 10, CompletionTokens: 20,
			RequestId: "request-1", Other: common.MapToJsonStr(map[string]interface{}{
				"cache_tokens": 3, "cache_write_tokens": 4,
			}),
		},
		{
			UserId: account.UserId, Type: LogTypeError, CreatedAt: 101, ModelName: "model-a",
			TokenName: "Production", Quota: 0, PromptTokens: 5, CompletionTokens: 0,
			RequestId: "request-2", Other: common.MapToJsonStr(map[string]interface{}{}),
		},
	}
	require.NoError(t, LOG_DB.Create(&logs).Error)

	page, total, totals, err := GetTensorGridUserUsage(
		account, 0, 0, "model-a", "", 0, 1, "", "", "", "",
	)

	require.NoError(t, err)
	assert.Len(t, page, 1)
	assert.Equal(t, int64(2), total)
	assert.Equal(t, int64(2), totals.RequestCount)
	assert.Equal(t, int64(1), totals.Successful)
	assert.Equal(t, int64(1), totals.Failed)
	assert.Equal(t, int64(15), totals.InputTokens)
	assert.Equal(t, int64(20), totals.OutputTokens)
	assert.Equal(t, int64(3), totals.CacheReadTokens)
	assert.Equal(t, int64(4), totals.CacheWriteTokens)
}

// 钱包余额上限跟随 common.MaxWalletQuota（64 位），而非单次请求的 int32 上限：
// 上游的充值/兑换路径已经允许余额越过 int32，TensorGrid 的入账与结算必须跟上，
// 否则余额一旦超过 int32 就会在扣费时报错，请求被免费放行且不产生入账事件。
func TestTensorGridWalletSupportsBalanceAboveSingleRequestBound(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "0f4c1d67-2f2e-4a5b-9d61-6b0f5a8e4c31"

	account, err := UpsertTensorGridAccount(subject, "whale@example.com", "Whale", "USD", "", true, 1)
	require.NoError(t, err)

	// $5,000 = 500_000 分 → 2_500_000_000 quota，超出 common.MaxQuota。
	credited, created, _, _, err := AdjustTensorGridBalance(subject, "payment:whale:0001", "USD", 500_000, 0, "large purchase", false)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 2_500_000_000, credited.BalanceQuota)
	assert.Greater(t, credited.BalanceQuota, common.MaxQuota)
	assert.Equal(t, int64(500_000), credited.BalanceMinor)
	assert.Equal(t, int64(5_000_000_000), credited.BalanceMicroUSD)

	// 余额高于 int32 时，单次扣费仍须正常结算并入账。
	handled, applied, err := AdjustTensorGridWalletQuota(
		account.UserId, "req:whale:0001", 1_000_000,
		RecordConsumeLogParams{ModelName: "whale-model"},
	)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 1_000_000, applied)

	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 2_499_000_000, user.Quota)

	var chargeOutbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "req:whale:0001").First(&chargeOutbox).Error)
	assert.Equal(t, int64(-2_000_000), chargeOutbox.DeltaMicroUSD)
	assert.Equal(t, int64(4_998_000_000), chargeOutbox.BalanceMicroUSD)
}
