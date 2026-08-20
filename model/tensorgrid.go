package model

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	TensorGridCurrencyUSD = "USD"
	TensorGridCurrencyIRT = "IRT"
)

var (
	ErrTensorGridAccountNotFound     = errors.New("tensorgrid account not found")
	ErrTensorGridCurrencyMismatch    = errors.New("tensorgrid account currency mismatch")
	ErrTensorGridIdempotencyConflict = errors.New("tensorgrid idempotency key conflict")
)

type TensorGridAccount struct {
	Id              int64     `json:"id" gorm:"primaryKey"`
	Subject         string    `json:"subject" gorm:"type:varchar(36);not null;uniqueIndex"`
	UserId          int       `json:"user_id" gorm:"not null;uniqueIndex"`
	SyncVersion     int64     `json:"sync_version" gorm:"not null;default:1"`
	CreditSequence  int64     `json:"credit_sequence" gorm:"not null;default:0"`
	Currency        string    `json:"currency" gorm:"type:varchar(3);not null"`
	FxRateIrtPerUSD string    `json:"fx_rate_irt_per_usd" gorm:"type:varchar(64);not null;default:'0'"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (TensorGridAccount) TableName() string { return "tensorgrid_accounts" }

// TensorGridCreditOutbox is the durable hand-off between Gateway settlement and
// TensorGrid's user-facing credit projection. It intentionally stores the
// authoritative balance after the mutation so a snapshot can repair a missed
// delivery without replaying a debit.
type TensorGridCreditOutbox struct {
	Id              int64      `json:"id" gorm:"primaryKey"`
	EventId         string     `json:"event_id" gorm:"type:varchar(128);not null;uniqueIndex"`
	Kind            string     `json:"kind" gorm:"type:varchar(16);not null;index"`
	Subject         string     `json:"subject" gorm:"type:varchar(36);not null;index"`
	AccountId       int64      `json:"account_id" gorm:"not null;index"`
	RequestId       string     `json:"request_id" gorm:"type:varchar(128);not null;default:'';index"`
	Sequence        int64      `json:"sequence" gorm:"not null;index"`
	Currency        string     `json:"currency" gorm:"type:varchar(3);not null"`
	DeltaMinor      int64      `json:"delta_minor" gorm:"not null;default:0"`
	DeltaMicroUSD   int64      `json:"delta_microusd" gorm:"not null;default:0"`
	BalanceMinor    int64      `json:"balance_minor" gorm:"not null;default:0"`
	BalanceMicroUSD int64      `json:"balance_microusd" gorm:"not null;default:0"`
	UsageBreakdown  string     `json:"usage_breakdown" gorm:"type:text;not null;default:'{}'"`
	PricingVersion  string     `json:"pricing_version" gorm:"type:varchar(128);not null;default:''"`
	OccurredAt      time.Time  `json:"occurred_at"`
	Attempts        int        `json:"attempts" gorm:"not null;default:0"`
	NextAttemptAt   time.Time  `json:"next_attempt_at" gorm:"index"`
	LastError       string     `json:"last_error" gorm:"type:text;not null;default:''"`
	DeliveredAt     *time.Time `json:"delivered_at" gorm:"index"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (TensorGridCreditOutbox) TableName() string { return "tensorgrid_credit_outboxes" }

const (
	TensorGridSettlementReserved = "reserved"
	TensorGridSettlementSettled  = "settled"
	TensorGridSettlementRefunded = "refunded"
)

// TensorGridBillingSettlement is the durable wallet reservation for a single
// Gateway request. The final wallet delta and credit outbox row are committed
// in one database transaction, so a process crash cannot leave a settled
// charge without a corresponding TensorGrid event.
type TensorGridBillingSettlement struct {
	Id                int64      `json:"id" gorm:"primaryKey"`
	AccountId         int64      `json:"account_id" gorm:"not null;index;uniqueIndex:tensorgrid_settlement_account_request"`
	RequestId         string     `json:"request_id" gorm:"type:varchar(128);not null;uniqueIndex:tensorgrid_settlement_account_request"`
	ReservedQuota     int        `json:"reserved_quota" gorm:"not null;default:0"`
	ActualQuota       int        `json:"actual_quota" gorm:"not null;default:0"`
	BalanceQuotaAfter int        `json:"balance_quota_after" gorm:"not null;default:0"`
	Status            string     `json:"status" gorm:"type:varchar(16);not null;index"`
	ReservedAt        time.Time  `json:"reserved_at"`
	SettledAt         *time.Time `json:"settled_at"`
	RefundedAt        *time.Time `json:"refunded_at"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (TensorGridBillingSettlement) TableName() string {
	return "tensorgrid_billing_settlements"
}

type TensorGridBalanceMutation struct {
	Id                    int64     `json:"id" gorm:"primaryKey"`
	AccountId             int64     `json:"account_id" gorm:"not null;index;uniqueIndex:tensorgrid_mutation_account_key"`
	IdempotencyKey        string    `json:"idempotency_key" gorm:"type:varchar(128);not null;uniqueIndex:tensorgrid_mutation_account_key"`
	AmountMinor           int64     `json:"amount_minor" gorm:"not null"`
	AmountMicroUSD        int64     `json:"amount_microusd" gorm:"not null;default:0"`
	AppliedAmountMinor    int64     `json:"applied_amount_minor" gorm:"not null"`
	AppliedAmountMicroUSD int64     `json:"applied_amount_microusd" gorm:"not null"`
	AllowPartialDebit     bool      `json:"allow_partial_debit" gorm:"not null"`
	Currency              string    `json:"currency" gorm:"type:varchar(3);not null"`
	QuotaDelta            int       `json:"quota_delta" gorm:"not null"`
	BalanceQuotaAfter     int       `json:"balance_quota_after" gorm:"not null"`
	Reason                string    `json:"reason" gorm:"type:varchar(255);not null;default:''"`
	CreatedAt             time.Time `json:"created_at"`
}

func (TensorGridBalanceMutation) TableName() string { return "tensorgrid_balance_mutations" }

type TensorGridTokenCreation struct {
	Id             int64     `json:"id" gorm:"primaryKey"`
	AccountId      int64     `json:"account_id" gorm:"not null;index;uniqueIndex:tensorgrid_token_creation_account_key"`
	IdempotencyKey string    `json:"idempotency_key" gorm:"type:varchar(128);not null;uniqueIndex:tensorgrid_token_creation_account_key"`
	TokenId        int       `json:"token_id" gorm:"not null;index"`
	Name           string    `json:"name" gorm:"type:varchar(50);not null"`
	ExpiresAt      int64     `json:"expires_at" gorm:"not null"`
	CreatedAt      time.Time `json:"created_at"`
}

func (TensorGridTokenCreation) TableName() string { return "tensorgrid_token_creations" }

type TensorGridBalanceSnapshot struct {
	Subject         string `json:"subject"`
	Currency        string `json:"currency"`
	BalanceMinor    int64  `json:"balance_minor"`
	BalanceMicroUSD int64  `json:"balance_microusd"`
	BalanceQuota    int    `json:"balance_quota"`
	QuotaPerUSD     string `json:"quota_per_usd"`
	FxRateIrtPerUSD string `json:"fx_rate_irt_per_usd"`
	SyncedAt        int64  `json:"synced_at"`
}

type TensorGridUsageTotals struct {
	RequestCount     int64            `json:"request_count"`
	Successful       int64            `json:"successful"`
	Failed           int64            `json:"failed"`
	InputTokens      int64            `json:"input_tokens"`
	OutputTokens     int64            `json:"output_tokens"`
	CacheReadTokens  int64            `json:"cache_read_tokens"`
	CacheWriteTokens int64            `json:"cache_write_tokens"`
	ChargeMicroUSD   map[string]int64 `json:"charge_microusd"`
	ChargeMinor      map[string]int64 `json:"charge_minor"`
}

func normalizeTensorGridSubject(subject string) (string, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(subject))
	if err != nil {
		return "", errors.New("invalid tensorgrid subject")
	}
	return parsed.String(), nil
}

func normalizeTensorGridCurrency(currency, fxRate string) (string, string, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency != TensorGridCurrencyUSD && currency != TensorGridCurrencyIRT {
		return "", "", errors.New("unsupported tensorgrid currency")
	}
	if currency == TensorGridCurrencyUSD {
		return currency, "1", nil
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(fxRate))
	if err != nil || !rate.IsPositive() {
		return "", "", errors.New("positive fx_rate_irt_per_usd is required for IRT")
	}
	return currency, rate.String(), nil
}

func tensorGridUsername(subject string) string {
	return "tg_" + strings.ReplaceAll(subject, "-", "")[:16]
}

func UpsertTensorGridAccount(subject, email, displayName, currency, fxRate string, enabled bool, syncVersion int64) (*TensorGridAccount, error) {
	normalizedSubject, err := normalizeTensorGridSubject(subject)
	if err != nil {
		return nil, err
	}
	if syncVersion < 1 {
		return nil, errors.New("positive tensorgrid sync version is required")
	}
	currency, fxRate, err = normalizeTensorGridCurrency(currency, fxRate)
	if err != nil {
		return nil, err
	}
	email = strings.TrimSpace(email)
	if len(email) > 50 {
		return nil, errors.New("tensorgrid email is too long")
	}
	displayName = strings.TrimSpace(displayName)
	displayNameRunes := []rune(displayName)
	if len(displayNameRunes) > 20 {
		displayName = string(displayNameRunes[:20])
	}
	status := common.UserStatusDisabled
	if enabled {
		status = common.UserStatusEnabled
	}

	var account TensorGridAccount
	var user User
	staleUpdate := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		err := lockForUpdate(tx).Where("subject = ?", normalizedSubject).First(&account).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			password, generateErr := common.GenerateKey()
			if generateErr != nil {
				return generateErr
			}
			user = User{
				Username: tensorGridUsername(normalizedSubject), Password: password,
				DisplayName: displayName, Email: email, Role: common.RoleCommonUser,
				Status: status, Group: "default",
			}
			if err := user.InsertWithTx(tx, 0); err != nil {
				return err
			}
			if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", 0).Error; err != nil {
				return err
			}
			user.Quota = 0
			account = TensorGridAccount{
				Subject: normalizedSubject, UserId: user.Id, SyncVersion: syncVersion,
				Currency: currency, FxRateIrtPerUSD: fxRate,
			}
			return tx.Create(&account).Error
		}
		if err != nil {
			return err
		}
		if syncVersion < account.SyncVersion {
			staleUpdate = true
			return nil
		}
		if err := lockForUpdate(tx).Where("id = ?", account.UserId).First(&user).Error; err != nil {
			return err
		}
		if account.Currency == TensorGridCurrencyIRT && currency == TensorGridCurrencyIRT &&
			account.FxRateIrtPerUSD != fxRate {
			balanceMicroUSD, conversionErr := quotaToMicroUSD(user.Quota)
			if conversionErr != nil {
				return conversionErr
			}
			balanceMinor, conversionErr := MicroUSDToTensorGridMinor(&account, balanceMicroUSD)
			if conversionErr != nil {
				return conversionErr
			}
			rebasedQuota := 0
			if balanceMinor != 0 {
				updatedAccount := account
				updatedAccount.FxRateIrtPerUSD = fxRate
				rebasedQuota, conversionErr = tensorGridAmountToQuota(balanceMinor, 0, &updatedAccount)
				if conversionErr != nil {
					return conversionErr
				}
			}
			if rebasedQuota != user.Quota {
				if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", rebasedQuota).Error; err != nil {
					return err
				}
				user.Quota = rebasedQuota
			}
		}
		if err := tx.Model(&User{}).Where("id = ?", user.Id).Updates(map[string]interface{}{
			"email": email, "display_name": displayName, "status": status,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&account).Updates(map[string]interface{}{
			"sync_version": syncVersion, "currency": currency, "fx_rate_irt_per_usd": fxRate,
		}).Error; err != nil {
			return err
		}
		account.Currency = currency
		account.FxRateIrtPerUSD = fxRate
		account.SyncVersion = syncVersion
		return nil
	})
	if err != nil {
		return nil, err
	}
	if staleUpdate {
		return &account, nil
	}
	if err := invalidateUserCache(user.Id); err != nil {
		common.SysLog("failed to invalidate TensorGrid user cache: " + err.Error())
	}
	if !enabled {
		if err := InvalidateUserTokensCache(user.Id); err != nil {
			common.SysLog("failed to invalidate disabled TensorGrid user tokens: " + err.Error())
		}
	}
	return &account, nil
}

func GetTensorGridAccount(subject string) (*TensorGridAccount, error) {
	normalizedSubject, err := normalizeTensorGridSubject(subject)
	if err != nil {
		return nil, err
	}
	var account TensorGridAccount
	if err := DB.Where("subject = ?", normalizedSubject).First(&account).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTensorGridAccountNotFound
		}
		return nil, err
	}
	return &account, nil
}

func quotaToMicroUSD(quota int) (int64, error) {
	if common.QuotaPerUnit <= 0 {
		return 0, errors.New("QuotaPerUnit must be positive")
	}
	value := decimal.NewFromInt(int64(quota)).Mul(decimal.NewFromInt(1_000_000)).
		Div(decimal.NewFromFloat(common.QuotaPerUnit)).Truncate(0)
	return value.IntPart(), nil
}

func QuotaToMicroUSDForTensorGrid(quota int) (int64, error) {
	return quotaToMicroUSD(quota)
}

func MicroUSDToTensorGridMinor(account *TensorGridAccount, microUSD int64) (int64, error) {
	if account.Currency == TensorGridCurrencyUSD {
		return microUSD / 10_000, nil
	}
	rate, err := decimal.NewFromString(account.FxRateIrtPerUSD)
	if err != nil || !rate.IsPositive() {
		return 0, errors.New("invalid TensorGrid account FX rate")
	}
	return decimal.NewFromInt(microUSD).Mul(rate).
		Div(decimal.NewFromInt(1_000_000)).Round(0).IntPart(), nil
}

func snapshotTensorGridBalance(account *TensorGridAccount, quota int) (*TensorGridBalanceSnapshot, error) {
	microUSD, err := quotaToMicroUSD(quota)
	if err != nil {
		return nil, err
	}
	balanceMinor, err := MicroUSDToTensorGridMinor(account, microUSD)
	if err != nil {
		return nil, err
	}
	return &TensorGridBalanceSnapshot{
		Subject: account.Subject, Currency: account.Currency, BalanceMinor: balanceMinor,
		BalanceMicroUSD: microUSD, BalanceQuota: quota,
		QuotaPerUSD:     decimal.NewFromFloat(common.QuotaPerUnit).String(),
		FxRateIrtPerUSD: account.FxRateIrtPerUSD, SyncedAt: time.Now().Unix(),
	}, nil
}

func GetTensorGridBalance(subject string) (*TensorGridBalanceSnapshot, error) {
	account, err := GetTensorGridAccount(subject)
	if err != nil {
		return nil, err
	}
	user, err := GetUserById(account.UserId, true)
	if err != nil {
		return nil, err
	}
	return snapshotTensorGridBalance(account, user.Quota)
}

func EnrichTensorGridLogOther(userId int, other map[string]interface{}) map[string]interface{} {
	var account TensorGridAccount
	if err := DB.Select("currency", "fx_rate_irt_per_usd").Where("user_id = ?", userId).First(&account).Error; err != nil {
		return other
	}
	if other == nil {
		other = make(map[string]interface{})
	}
	other["tensorgrid_billing"] = map[string]interface{}{
		"currency": account.Currency, "fx_rate_irt_per_usd": account.FxRateIrtPerUSD,
	}
	return other
}

func tensorGridLogNumber(other map[string]interface{}, key string) int64 {
	switch value := other[key].(type) {
	case float64:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	case string:
		parsed, _ := strconv.ParseInt(value, 10, 64)
		return parsed
	default:
		return 0
	}
}

func tensorGridLogBilling(log *Log, fallback *TensorGridAccount) (string, string) {
	currency := fallback.Currency
	fxRate := fallback.FxRateIrtPerUSD
	other, _ := common.StrToMap(log.Other)
	billing, _ := other["tensorgrid_billing"].(map[string]interface{})
	if value, ok := billing["currency"].(string); ok &&
		(value == TensorGridCurrencyUSD || value == TensorGridCurrencyIRT) {
		currency = value
	}
	if value, ok := billing["fx_rate_irt_per_usd"].(string); ok && value != "" {
		fxRate = value
	}
	return currency, fxRate
}

func GetTensorGridUserUsage(
	account *TensorGridAccount, startTimestamp, endTimestamp int64, modelName, tokenName string,
	startIdx, num int, requestId, status, source, stream string,
) ([]*Log, int64, TensorGridUsageTotals, error) {
	totals := TensorGridUsageTotals{
		ChargeMicroUSD: map[string]int64{},
		ChargeMinor:    map[string]int64{},
	}
	logTypes := []int{LogTypeConsume, LogTypeError}
	if status == "succeeded" {
		logTypes = []int{LogTypeConsume}
	} else if status == "failed" {
		logTypes = []int{LogTypeError}
	} else if status != "" {
		logTypes = []int{-1}
	}
	tx := LOG_DB.Where("logs.user_id = ? AND logs.type IN ?", account.UserId, logTypes)
	var err error
	if tx, err = applyExplicitLogTextFilter(tx, "logs.model_name", modelName); err != nil {
		return nil, 0, totals, err
	}
	if tokenName != "" {
		tx = tx.Where("logs.token_name = ?", tokenName)
	}
	if source == "playground" {
		tx = tx.Where("logs.token_name = ?", "TensorGrid Playground")
	} else if source == "api" {
		tx = tx.Where("logs.token_name <> ?", "TensorGrid Playground")
	} else if source != "" {
		tx = tx.Where("1 = 0")
	}
	if stream == "true" {
		tx = tx.Where("logs.is_stream = ?", true)
	} else if stream == "false" {
		tx = tx.Where("logs.is_stream = ?", false)
	} else if stream != "" {
		tx = tx.Where("1 = 0")
	}
	if requestId != "" {
		tx = tx.Where("logs.request_id = ?", requestId)
	}
	if startTimestamp != 0 {
		tx = tx.Where("logs.created_at >= ?", startTimestamp)
	}
	if endTimestamp != 0 {
		tx = tx.Where("logs.created_at <= ?", endTimestamp)
	}
	order := "logs.id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = clickHouseLogOrder("logs.")
	}
	base := tx.Model(&Log{})
	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, totals, errors.New("failed to query TensorGrid usage")
	}
	totalRows, err := base.Select(
		"logs.type", "logs.quota", "logs.prompt_tokens", "logs.completion_tokens", "logs.other",
	).Rows()
	if err != nil {
		return nil, 0, totals, errors.New("failed to query TensorGrid usage totals")
	}
	defer totalRows.Close()
	for totalRows.Next() {
		var log Log
		if err := LOG_DB.ScanRows(totalRows, &log); err != nil {
			return nil, 0, totals, errors.New("failed to scan TensorGrid usage totals")
		}
		totals.RequestCount++
		if log.Type == LogTypeConsume {
			totals.Successful++
		} else {
			totals.Failed++
		}
		totals.InputTokens += int64(log.PromptTokens)
		totals.OutputTokens += int64(log.CompletionTokens)
		other, _ := common.StrToMap(log.Other)
		totals.CacheReadTokens += tensorGridLogNumber(other, "cache_tokens")
		totals.CacheWriteTokens += tensorGridLogNumber(other, "cache_write_tokens")
		chargeMicroUSD, conversionErr := quotaToMicroUSD(log.Quota)
		if conversionErr != nil {
			return nil, 0, totals, conversionErr
		}
		currency, fxRate := tensorGridLogBilling(&log, account)
		totals.ChargeMicroUSD[currency] += chargeMicroUSD
		billingAccount := *account
		billingAccount.Currency = currency
		billingAccount.FxRateIrtPerUSD = fxRate
		chargeMinor, conversionErr := MicroUSDToTensorGridMinor(&billingAccount, chargeMicroUSD)
		if conversionErr != nil {
			return nil, 0, totals, conversionErr
		}
		totals.ChargeMinor[currency] += chargeMinor
	}
	if err := totalRows.Err(); err != nil {
		return nil, 0, totals, errors.New("failed to read TensorGrid usage totals")
	}
	if startIdx < 0 {
		startIdx = 0
	}
	if num < 0 {
		num = 0
	}
	if int64(startIdx) >= total || num == 0 {
		return []*Log{}, total, totals, nil
	}
	var page []*Log
	if err := base.Select(
		"logs.id", "logs.user_id", "logs.created_at", "logs.type", "logs.token_name",
		"logs.model_name", "logs.quota", "logs.prompt_tokens", "logs.completion_tokens",
		"logs.use_time", "logs.is_stream", "logs.request_id", "logs.other",
	).Order(order).Offset(startIdx).Limit(num).Find(&page).Error; err != nil {
		return nil, 0, totals, errors.New("failed to query TensorGrid usage page")
	}
	formatUserLogs(page, startIdx)
	return page, total, totals, nil
}

func tensorGridAmountToQuota(amountMinor, amountMicroUSD int64, account *TensorGridAccount) (int, error) {
	if (amountMinor == 0) == (amountMicroUSD == 0) {
		return 0, errors.New("provide exactly one non-zero amount_minor or amount_microusd")
	}
	amountMicroUSDDecimal := decimal.NewFromInt(amountMicroUSD)
	if amountMinor != 0 {
		amountMicroUSDDecimal = decimal.NewFromInt(amountMinor).Mul(decimal.NewFromInt(10_000))
	}
	if amountMicroUSD != 0 && account.Currency != TensorGridCurrencyUSD {
		return 0, errors.New("amount_microusd is only valid for USD accounts")
	}
	if amountMinor != 0 && account.Currency == TensorGridCurrencyIRT {
		rate, err := decimal.NewFromString(account.FxRateIrtPerUSD)
		if err != nil || !rate.IsPositive() {
			return 0, errors.New("invalid TensorGrid account FX rate")
		}
		amountMicroUSDDecimal = decimal.NewFromInt(amountMinor).Mul(decimal.NewFromInt(1_000_000)).Div(rate)
	}
	quotaDecimal := amountMicroUSDDecimal.Mul(decimal.NewFromFloat(common.QuotaPerUnit)).Div(decimal.NewFromInt(1_000_000))
	quota, err := common.QuotaFromDecimalStrict(quotaDecimal)
	if err != nil {
		return 0, err
	}
	if quota == 0 {
		return 0, errors.New("amount_minor is below the minimum representable gateway quota")
	}
	return quota, nil
}

func AdjustTensorGridBalance(
	subject, idempotencyKey, currency string, amountMinor, amountMicroUSD int64,
	reason string, allowPartialDebit bool,
) (*TensorGridBalanceSnapshot, bool, int64, int64, error) {
	if common.BatchUpdateEnabled {
		return nil, false, 0, 0, errors.New("TensorGrid integration requires BATCH_UPDATE_ENABLED=false")
	}
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		return nil, false, 0, 0, errors.New("invalid idempotency key")
	}
	account, err := GetTensorGridAccount(subject)
	if err != nil {
		return nil, false, 0, 0, err
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	reason = strings.TrimSpace(reason)
	if len(reason) > 255 {
		reason = reason[:255]
	}
	created := false
	var balanceAfter int
	var appliedAmountMinor int64
	var appliedAmountMicroUSD int64
	err = DB.Transaction(func(tx *gorm.DB) error {
		var lockedAccount TensorGridAccount
		if err := lockForUpdate(tx).Where("id = ?", account.Id).First(&lockedAccount).Error; err != nil {
			return err
		}
		account = &lockedAccount
		if currency != account.Currency {
			return ErrTensorGridCurrencyMismatch
		}
		quotaDelta, err := tensorGridAmountToQuota(amountMinor, amountMicroUSD, account)
		if err != nil {
			return err
		}
		var existing TensorGridBalanceMutation
		existingErr := tx.Where(
			"account_id = ? AND idempotency_key = ?", account.Id, idempotencyKey,
		).First(&existing).Error
		if existingErr == nil {
			if existing.AccountId != account.Id || existing.AmountMinor != amountMinor ||
				existing.AmountMicroUSD != amountMicroUSD || existing.Currency != currency ||
				existing.AllowPartialDebit != allowPartialDebit {
				return ErrTensorGridIdempotencyConflict
			}
			appliedAmountMinor = existing.AppliedAmountMinor
			appliedAmountMicroUSD = existing.AppliedAmountMicroUSD
			var currentUser User
			if err := lockForUpdate(tx).Where("id = ?", account.UserId).First(&currentUser).Error; err != nil {
				return err
			}
			balanceAfter = currentUser.Quota
			return nil
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", account.UserId).First(&user).Error; err != nil {
			return err
		}
		appliedQuotaDelta := quotaDelta
		if quotaDelta < 0 && user.Quota < -quotaDelta {
			if !allowPartialDebit {
				return errors.New("insufficient gateway balance")
			}
			appliedQuotaDelta = -user.Quota
		}
		beforeSnapshot, err := snapshotTensorGridBalance(account, user.Quota)
		if err != nil {
			return err
		}
		balance64 := int64(user.Quota) + int64(appliedQuotaDelta)
		if balance64 < 0 || balance64 > int64(common.MaxQuota) {
			return errors.New("gateway balance is outside the supported range")
		}
		balanceAfter = int(balance64)
		afterSnapshot, err := snapshotTensorGridBalance(account, balanceAfter)
		if err != nil {
			return err
		}
		appliedAmountMinor = afterSnapshot.BalanceMinor - beforeSnapshot.BalanceMinor
		appliedAmountMicroUSD = afterSnapshot.BalanceMicroUSD - beforeSnapshot.BalanceMicroUSD
		if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", balanceAfter).Error; err != nil {
			return err
		}
		mutation := TensorGridBalanceMutation{
			AccountId: account.Id, IdempotencyKey: idempotencyKey, AmountMinor: amountMinor,
			AmountMicroUSD: amountMicroUSD, AppliedAmountMinor: appliedAmountMinor,
			AppliedAmountMicroUSD: appliedAmountMicroUSD, AllowPartialDebit: allowPartialDebit,
			Currency: currency, QuotaDelta: appliedQuotaDelta,
			BalanceQuotaAfter: balanceAfter, Reason: reason,
		}
		if err := tx.Create(&mutation).Error; err != nil {
			return err
		}
		usageJSON, err := tensorGridUsageJSON(map[string]interface{}{
			"reason": reason, "source": "tensorgrid_adjustment",
		})
		if err != nil {
			return err
		}
		if err := enqueueTensorGridCreditEventTx(
			tx, account, balanceAfter, "adjust:"+idempotencyKey,
			appliedAmountMinor, appliedAmountMicroUSD, usageJSON, "",
		); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, 0, 0, err
	}
	if err := invalidateUserCache(account.UserId); err != nil {
		common.SysLog(fmt.Sprintf("failed to invalidate TensorGrid balance cache for user %d: %v", account.UserId, err))
	}
	snapshot, err := snapshotTensorGridBalance(account, balanceAfter)
	return snapshot, created, appliedAmountMinor, appliedAmountMicroUSD, err
}

func CreateTensorGridToken(subject, idempotencyKey, name string, expiresAt int64) (*Token, bool, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 128 {
		return nil, false, errors.New("invalid idempotency key")
	}
	account, err := GetTensorGridAccount(subject)
	if err != nil {
		return nil, false, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 50 {
		return nil, false, errors.New("token name must contain 1 to 50 characters")
	}
	if expiresAt != 0 && expiresAt <= common.GetTimestamp() {
		return nil, false, errors.New("token expiry must be in the future")
	}
	if expiresAt == 0 {
		expiresAt = -1
	}

	created := false
	var token Token
	err = DB.Transaction(func(tx *gorm.DB) error {
		var lockedAccount TensorGridAccount
		if err := lockForUpdate(tx).Where("id = ?", account.Id).First(&lockedAccount).Error; err != nil {
			return err
		}
		var existing TensorGridTokenCreation
		existingErr := tx.Where(
			"account_id = ? AND idempotency_key = ?", account.Id, idempotencyKey,
		).First(&existing).Error
		if existingErr == nil {
			if existing.Name != name || existing.ExpiresAt != expiresAt {
				return ErrTensorGridIdempotencyConflict
			}
			return tx.Where("id = ? AND user_id = ?", existing.TokenId, account.UserId).First(&token).Error
		}
		if !errors.Is(existingErr, gorm.ErrRecordNotFound) {
			return existingErr
		}
		var count int64
		if err := tx.Model(&Token{}).Where("user_id = ?", account.UserId).Count(&count).Error; err != nil {
			return err
		}
		if count >= int64(operation_setting.GetMaxUserTokens()) {
			return errors.New("maximum active token count reached")
		}
		key, err := common.GenerateKey()
		if err != nil {
			return err
		}
		now := common.GetTimestamp()
		token = Token{
			UserId: account.UserId, Name: name, Key: key, Status: common.TokenStatusEnabled,
			CreatedTime: now, AccessedTime: now, ExpiredTime: expiresAt,
			UnlimitedQuota: true, Group: "default",
		}
		if err := tx.Create(&token).Error; err != nil {
			return err
		}
		creation := TensorGridTokenCreation{
			AccountId: account.Id, IdempotencyKey: idempotencyKey, TokenId: token.Id,
			Name: name, ExpiresAt: expiresAt,
		}
		if err := tx.Create(&creation).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return &token, created, nil
}
