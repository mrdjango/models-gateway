package model

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	tensorGridFxRateRowID = 1
	// tensorGridFxDefaultMaxStale mirrors TensorGrid's fx.max_stale_seconds
	// default, for a pushed rate that did not carry its own threshold.
	tensorGridFxDefaultMaxStale   = 900 * time.Second
	tensorGridFxStaleWarnInterval = 5 * time.Minute
)

var tensorGridFxStaleWarnedAt atomic.Int64

// Stale reports whether the rate is older than TensorGrid's own staleness
// threshold, which means its exchange-rate worker has stopped delivering.
func (r *TensorGridFxRate) Stale(now time.Time) bool {
	maxStale := time.Duration(r.MaxStaleSeconds) * time.Second
	if maxStale <= 0 {
		maxStale = tensorGridFxDefaultMaxStale
	}
	return now.Sub(r.FetchedAt) > maxStale
}

// SetTensorGridFxRate stores TensorGrid's latest IRT-per-USD snapshot. Pushes
// can arrive out of order, so one that is not newer than the stored snapshot is
// ignored. Accounts are not rebased here: each IRT account moves onto the new
// rate inside the transaction of its next wallet mutation.
func SetTensorGridFxRate(rate string, snapshotID int64, fetchedAt time.Time, maxStaleSeconds int) (*TensorGridFxRate, bool, error) {
	_, normalized, err := normalizeTensorGridCurrency(TensorGridCurrencyIRT, rate)
	if err != nil {
		return nil, false, err
	}
	if fetchedAt.IsZero() {
		return nil, false, errors.New("fetched_at is required")
	}
	if maxStaleSeconds < 0 {
		return nil, false, errors.New("max_stale_seconds cannot be negative")
	}
	fetchedAt = fetchedAt.UTC()

	var stored TensorGridFxRate
	applied := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		lookupErr := lockForUpdate(tx).Where("id = ?", tensorGridFxRateRowID).First(&stored).Error
		if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		if lookupErr == nil && !fetchedAt.After(stored.FetchedAt) {
			return nil
		}
		stored = TensorGridFxRate{
			Id: tensorGridFxRateRowID, RateIrtPerUSD: normalized, SnapshotId: snapshotID,
			FetchedAt: fetchedAt, MaxStaleSeconds: maxStaleSeconds,
		}
		applied = true
		return tx.Save(&stored).Error
	})
	if err != nil {
		return nil, false, err
	}
	return &stored, applied, nil
}

// GetTensorGridFxRate returns the pushed rate, or nil before the first push.
func GetTensorGridFxRate() (*TensorGridFxRate, error) {
	return getTensorGridFxRateTx(DB)
}

func getTensorGridFxRateTx(tx *gorm.DB) (*TensorGridFxRate, error) {
	var rate TensorGridFxRate
	err := tx.Where("id = ?", tensorGridFxRateRowID).First(&rate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rate, nil
}

func tensorGridRatesEqual(left, right string) bool {
	leftRate, leftErr := decimal.NewFromString(left)
	rightRate, rightErr := decimal.NewFromString(right)
	if leftErr != nil || rightErr != nil {
		return left == right
	}
	return leftRate.Equal(rightRate)
}

// applyCurrentTensorGridFxRateTx moves an IRT account onto the Gateway-wide
// rate before its wallet changes, so the USD amount about to be charged or
// credited is converted at today's rate. The caller must hold the account and
// user row locks. Before TensorGrid's first push the account keeps its own rate.
func applyCurrentTensorGridFxRateTx(tx *gorm.DB, account *TensorGridAccount, user *User) error {
	if account.Currency != TensorGridCurrencyIRT {
		return nil
	}
	current, err := getTensorGridFxRateTx(tx)
	if err != nil || current == nil {
		return err
	}
	warnIfTensorGridFxRateStale(current, time.Now())
	if tensorGridRatesEqual(account.FxRateIrtPerUSD, current.RateIrtPerUSD) {
		return nil
	}
	return rebaseTensorGridAccountTx(tx, account, user, current.RateIrtPerUSD)
}

// rebaseTensorGridAccountTx moves an IRT account onto newRate while keeping its
// IRT balance: the USD-denominated quota is re-derived from the IRT amount the
// user holds at the old rate. The caller must hold both row locks.
func rebaseTensorGridAccountTx(tx *gorm.DB, account *TensorGridAccount, user *User, newRate string) error {
	rate, err := decimal.NewFromString(newRate)
	if err != nil || !rate.IsPositive() {
		return errors.New("invalid TensorGrid account FX rate")
	}
	balanceMicroUSD, err := quotaToMicroUSD(user.Quota)
	if err != nil {
		return err
	}
	balanceMinor, err := MicroUSDToTensorGridMinor(account, balanceMicroUSD)
	if err != nil {
		return err
	}
	// Same arithmetic as tensorGridAmountToQuota, except that a balance too
	// small to survive the new rate rounds to zero instead of failing the
	// mutation that triggered the rebase.
	rebasedQuota, err := common.WalletQuotaFromDecimalStrict(decimal.NewFromInt(balanceMinor).
		Mul(decimal.NewFromInt(1_000_000)).Div(rate).
		Mul(decimal.NewFromFloat(common.QuotaPerUnit)).Div(decimal.NewFromInt(1_000_000)))
	if err != nil {
		return err
	}
	if rebasedQuota != user.Quota {
		if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", rebasedQuota).Error; err != nil {
			return err
		}
		user.Quota = rebasedQuota
	}
	if err := tx.Model(&TensorGridAccount{}).Where("id = ?", account.Id).
		Update("fx_rate_irt_per_usd", newRate).Error; err != nil {
		return err
	}
	account.FxRateIrtPerUSD = newRate
	return nil
}

// warnIfTensorGridFxRateStale keeps charging at the last rate, by product
// decision, but says so in the system log at most once per interval.
func warnIfTensorGridFxRateStale(rate *TensorGridFxRate, now time.Time) {
	if !rate.Stale(now) {
		return
	}
	last := tensorGridFxStaleWarnedAt.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < tensorGridFxStaleWarnInterval {
		return
	}
	if !tensorGridFxStaleWarnedAt.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	common.SysLog(fmt.Sprintf(
		"TensorGrid FX rate is stale: %s IRT/USD from snapshot %d fetched at %s; IRT wallets are still charged at it until TensorGrid pushes a fresh rate",
		rate.RateIrtPerUSD, rate.SnapshotId, rate.FetchedAt.UTC().Format(time.RFC3339),
	))
}
