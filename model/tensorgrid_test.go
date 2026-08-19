package model

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupTensorGridModelTest(t *testing.T) {
	t.Helper()
	previousDB := DB
	previousLogDB := LOG_DB
	previousRedis := common.RedisEnabled
	previousBatch := common.BatchUpdateEnabled
	previousQuotaPerUnit := common.QuotaPerUnit
	previousOptionMap := common.OptionMap
	previousBillingModes := billing_setting.GetBillingModeCopy()
	previousBillingExpressions := billing_setting.GetBillingExprCopy()

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
		&TensorGridBalanceMutation{}, &TensorGridTokenCreation{}, &TensorGridCatalogSync{},
	))

	t.Cleanup(func() {
		if modes, marshalErr := common.Marshal(previousBillingModes); marshalErr == nil {
			_ = UpdateOption("billing_setting.billing_mode", string(modes))
		}
		if expressions, marshalErr := common.Marshal(previousBillingExpressions); marshalErr == nil {
			_ = UpdateOption("billing_setting.billing_expr", string(expressions))
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

	_, _, _, _, err = AdjustTensorGridBalance(subject, "payment:00000001", "USD", 200, 0, "conflict", false)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)

	debited, created, _, _, err := AdjustTensorGridBalance(subject, "compute:00000001", "USD", -25, 0, "compute usage", false)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(75), debited.BalanceMinor)
	assert.Equal(t, 375_000, debited.BalanceQuota)
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

	_, _, err = CreateTensorGridToken(subject, "token:00000001", "Different", 0)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)
}

func TestTensorGridCatalogSyncCreatesMappedChannelAndTieredPricingIdempotently(t *testing.T) {
	setupTensorGridModelTest(t)
	expression := `v1:tier("base", p * 1 + c * 2 + cr * 0.1 + cc * 1.25 + cc1h * 1.25)`
	channels := []TensorGridCatalogChannel{{
		ExternalId: "provider:primary", BaseURL: "https://provider.example.test", Key: "provider-secret",
		Enabled: true,
		Models: []TensorGridCatalogModel{{
			PublicId: "tg:primary:model", UpstreamModelId: "upstream/model", BillingExpr: expression,
		}},
	}}

	result, created, err := SyncTensorGridCatalog(
		"catalog:00000001", strings.Repeat("a", 64), "revision-1", channels,
	)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, 1, result.ChannelCount)
	assert.Equal(t, 1, result.ModelCount)

	var channel Channel
	require.NoError(t, DB.Where("remark = ?", tensorGridManagedChannelRemark).First(&channel).Error)
	assert.Equal(t, "tg:primary:model", channel.Models)
	assert.Contains(t, channel.GetModelMapping(), `"tg:primary:model":"upstream/model"`)
	assert.Equal(t, expression, billing_setting.GetBillingExprCopy()["tg:primary:model"])
	assert.Equal(t, billing_setting.BillingModeTieredExpr, billing_setting.GetBillingMode("tg:primary:model"))

	replayed, created, err := SyncTensorGridCatalog(
		"catalog:00000001", strings.Repeat("a", 64), "revision-1", channels,
	)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, result, replayed)

	_, _, err = SyncTensorGridCatalog(
		"catalog:00000001", strings.Repeat("b", 64), "revision-1", channels,
	)
	assert.ErrorIs(t, err, ErrTensorGridIdempotencyConflict)
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
