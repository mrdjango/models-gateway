package model

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedIrtTensorGridAccount opens an IRT account at 100,000 IRT/USD holding
// 10,000,000 IRT ($100), which is 50,000,000 quota.
func seedIrtTensorGridAccount(t *testing.T, subject string) *TensorGridAccount {
	t.Helper()
	account, err := UpsertTensorGridAccount(subject, "irt@example.com", "Irt", "IRT", "100000", true, 1)
	require.NoError(t, err)
	seeded, _, _, _, err := AdjustTensorGridBalance(subject, "seed:"+subject, "IRT", 10_000_000, 0, "seed", false)
	require.NoError(t, err)
	require.Equal(t, int64(10_000_000), seeded.BalanceMinor)
	require.Equal(t, 50_000_000, seeded.BalanceQuota)
	return account
}

func requireTensorGridBalanceMinor(t *testing.T, subject string, expected int64) *TensorGridBalanceSnapshot {
	t.Helper()
	balance, err := GetTensorGridBalance(subject)
	require.NoError(t, err)
	assert.Equal(t, expected, balance.BalanceMinor)
	return balance
}

// chargeOneDollar reserves and settles 500,000 quota ($1) the way a relayed
// request does, and returns the IRT delta its credit event carries.
func chargeOneDollar(t *testing.T, userID int, requestID string) int64 {
	t.Helper()
	handled, reserved, err := ReserveTensorGridWalletQuota(userID, requestID, 500_000)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, reserved)
	handled, err = SettleTensorGridWalletQuota(userID, requestID, 500_000, RecordConsumeLogParams{ModelName: "fx-model"})
	require.NoError(t, err)
	require.True(t, handled)
	var outbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", requestID).First(&outbox).Error)
	return outbox.DeltaMinor
}

func TestTensorGridIrtWalletChargesAtPushedRateWithoutAccountSync(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "0b6f2a4e-5d1c-4e8a-9f3b-2c7d8e1a4b50"
	account := seedIrtTensorGridAccount(t, subject)

	// TensorGrid never syncs this user again (an API-key-only customer); only
	// the Gateway-wide rate moves.
	_, applied, err := SetTensorGridFxRate("120000", 2, time.Now(), 900)
	require.NoError(t, err)
	require.True(t, applied)

	assert.Equal(t, int64(-120_000), chargeOneDollar(t, account.UserId, "fx-pushed-rate"))
	requireTensorGridBalanceMinor(t, subject, 9_880_000)
	refreshed, err := GetTensorGridAccount(subject)
	require.NoError(t, err)
	assert.Equal(t, "120000", refreshed.FxRateIrtPerUSD)

	// Task reconciliation and violation fees convert at the same rate.
	handled, appliedDelta, err := AdjustTensorGridWalletQuota(
		account.UserId, "fx-task-charge", 500_000, RecordConsumeLogParams{ModelName: "fx-task"},
	)
	require.NoError(t, err)
	require.True(t, handled)
	assert.Equal(t, 500_000, appliedDelta)
	var taskOutbox TensorGridCreditOutbox
	require.NoError(t, DB.Where("request_id = ?", "fx-task-charge").First(&taskOutbox).Error)
	assert.Equal(t, int64(-120_000), taskOutbox.DeltaMinor)
	requireTensorGridBalanceMinor(t, subject, 9_760_000)
}

func TestTensorGridIrtWalletKeepsAccountRateBeforeFirstPush(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "6c1e9b3a-7f2d-4a5e-8b0c-3d9e2f1a6c74"
	account := seedIrtTensorGridAccount(t, subject)

	assert.Equal(t, int64(-100_000), chargeOneDollar(t, account.UserId, "fx-no-push"))
	requireTensorGridBalanceMinor(t, subject, 9_900_000)
}

func TestTensorGridIrtWalletKeepsChargingAtStaleRate(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "9a4d2c7e-1b3f-4e6a-8d5c-0f2e7b9a1c38"
	account := seedIrtTensorGridAccount(t, subject)

	stored, applied, err := SetTensorGridFxRate("120000", 3, time.Now().Add(-2*time.Hour), 900)
	require.NoError(t, err)
	require.True(t, applied)
	assert.True(t, stored.Stale(time.Now()))

	assert.Equal(t, int64(-120_000), chargeOneDollar(t, account.UserId, "fx-stale-rate"))
	requireTensorGridBalanceMinor(t, subject, 9_880_000)
}

func TestTensorGridFxRateIgnoresOutOfOrderPushes(t *testing.T) {
	setupTensorGridModelTest(t)
	newer := time.Now().Truncate(time.Second)

	stored, applied, err := SetTensorGridFxRate("120000.000000", 5, newer, 900)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, "120000", stored.RateIrtPerUSD)
	assert.False(t, stored.Stale(time.Now()))

	stored, applied, err = SetTensorGridFxRate("90000", 4, newer.Add(-time.Minute), 900)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, "120000", stored.RateIrtPerUSD)

	_, applied, err = SetTensorGridFxRate("90000", 5, newer, 900)
	require.NoError(t, err)
	assert.False(t, applied)

	stored, applied, err = SetTensorGridFxRate("125000", 6, newer.Add(time.Minute), 600)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Equal(t, "125000", stored.RateIrtPerUSD)
	assert.Equal(t, int64(6), stored.SnapshotId)

	current, err := GetTensorGridFxRate()
	require.NoError(t, err)
	assert.Equal(t, "125000", current.RateIrtPerUSD)
	assert.Equal(t, 600, current.MaxStaleSeconds)

	for _, invalid := range []string{"", "0", "-1", "abc", "1e1000"} {
		_, _, err = SetTensorGridFxRate(invalid, 7, newer.Add(2*time.Minute), 900)
		assert.Error(t, err, invalid)
	}
}

func TestTensorGridUsdWalletIgnoresFxRate(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "3e8b1d6a-2c4f-4a9e-b7d0-5f1c8e3a2b96"
	account, err := UpsertTensorGridAccount(subject, "usd@example.com", "Usd", "USD", "", true, 1)
	require.NoError(t, err)
	_, _, _, _, err = AdjustTensorGridBalance(subject, "seed:usd-fx", "USD", 200, 0, "seed", false)
	require.NoError(t, err)
	_, _, err = SetTensorGridFxRate("120000", 8, time.Now(), 900)
	require.NoError(t, err)

	chargeOneDollar(t, account.UserId, "fx-usd")
	user, err := GetUserById(account.UserId, true)
	require.NoError(t, err)
	assert.Equal(t, 500_000, user.Quota)
	refreshed, err := GetTensorGridAccount(subject)
	require.NoError(t, err)
	assert.Equal(t, "1", refreshed.FxRateIrtPerUSD)
}

func TestTensorGridIrtTopUpAfterRateChangeCreditsExactIrt(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "7d2a5f8c-4b1e-4c3a-9e6d-1a0b3c5e7f29"
	seedIrtTensorGridAccount(t, subject)
	_, _, err := SetTensorGridFxRate("120000", 9, time.Now(), 900)
	require.NoError(t, err)

	balance, created, appliedMinor, _, err := AdjustTensorGridBalance(
		subject, "payment:fx-top-up", "IRT", 1_200_000, 0, "purchase", false,
	)
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, int64(1_200_000), appliedMinor)
	assert.Equal(t, int64(11_200_000), balance.BalanceMinor)
	assert.Equal(t, "120000", balance.FxRateIrtPerUSD)
}

func TestTensorGridUpsertPrefersPushedRateOverPayloadRate(t *testing.T) {
	setupTensorGridModelTest(t)
	const subject = "2f9c4e1b-8a3d-4b7e-a5c2-6d0e9f1b3a47"
	seedIrtTensorGridAccount(t, subject)
	_, _, err := SetTensorGridFxRate("120000", 10, time.Now(), 900)
	require.NoError(t, err)

	// TensorGrid's per-user sync may carry an older rate than the pushed one.
	account, err := UpsertTensorGridAccount(subject, "irt@example.com", "Irt", "IRT", "100000", true, 2)
	require.NoError(t, err)
	assert.Equal(t, "120000", account.FxRateIrtPerUSD)
	balance := requireTensorGridBalanceMinor(t, subject, 10_000_000)
	assert.Equal(t, 41_666_667, balance.BalanceQuota)
}
