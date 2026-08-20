package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

const (
	tensorGridCreditSyncIntervalEnv = "TENSORGRID_CREDIT_SYNC_INTERVAL_SECONDS"
	tensorGridSnapshotIntervalEnv   = "TENSORGRID_CREDIT_SNAPSHOT_INTERVAL_SECONDS"
	tensorGridReservationTimeoutEnv = "TENSORGRID_RESERVATION_TIMEOUT_SECONDS"
	tensorGridCallbackBaseURLEnv    = "TENSORGRID_CALLBACK_BASE_URL"
	tensorGridCallbackSecretEnv     = "TENSORGRID_INTEGRATION_SECRET"
)

var tensorGridSyncOnce sync.Once

func tensorGridSyncConfig() (string, string, time.Duration, time.Duration) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv(tensorGridCallbackBaseURLEnv)), "/")
	secret := os.Getenv(tensorGridCallbackSecretEnv)
	interval := envDuration(tensorGridCreditSyncIntervalEnv, 1)
	snapshotInterval := envDuration(tensorGridSnapshotIntervalEnv, 300)
	return baseURL, secret, interval, snapshotInterval
}

func ValidateTensorGridCreditSyncConfig() error {
	baseURL, secret, _, _ := tensorGridSyncConfig()
	if len(secret) < 32 {
		return errors.New("TENSORGRID_INTEGRATION_SECRET must contain at least 32 characters")
	}
	parsedURL, err := url.Parse(baseURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" {
		return errors.New("TENSORGRID_CALLBACK_BASE_URL must be an absolute HTTP(S) URL")
	}
	if strings.TrimSpace(os.Getenv("NODE_TYPE")) != "master" {
		return errors.New("TensorGrid integration requires NODE_TYPE=master")
	}
	if strings.TrimSpace(os.Getenv("BATCH_UPDATE_ENABLED")) != "false" {
		return errors.New("TensorGrid integration requires BATCH_UPDATE_ENABLED=false")
	}
	return nil
}

func envDuration(name string, fallbackSeconds int) time.Duration {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 1 {
		value = fallbackSeconds
	}
	return time.Duration(value) * time.Second
}

// StartTensorGridCreditSync runs on the Gateway master only. The durable
// outbox makes process crashes harmless; the short delivery interval provides
// near-realtime updates while the snapshot interval repairs any missed event.
func StartTensorGridCreditSync() {
	tensorGridSyncOnce.Do(func() {
		if !common.IsMasterNode {
			return
		}
		go func() {
			_, _, interval, snapshotInterval := tensorGridSyncConfig()
			reservationTimeout := envDuration(tensorGridReservationTimeoutEnv, 7200)
			lastSnapshot := time.Time{}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				now := time.Now()
				refunded, refundFailed, refundErr := model.RefundStaleTensorGridWalletReservations(
					now.Add(-reservationTimeout), 100,
				)
				if refundErr != nil {
					common.SysLog("TensorGrid stale reservation recovery failed: " + refundErr.Error())
				} else if refunded > 0 || refundFailed > 0 {
					common.SysLog(fmt.Sprintf("TensorGrid stale reservation recovery: refunded=%d failed=%d", refunded, refundFailed))
				}
				replayed, failed, reconcileErr := model.ReconcileTensorGridCreditEvents(1000)
				if reconcileErr != nil {
					common.SysLog("TensorGrid credit reconciliation failed: " + reconcileErr.Error())
				} else if failed > 0 {
					common.SysLog(fmt.Sprintf("TensorGrid credit reconciliation: replayed=%d failed=%d", replayed, failed))
				}
				if now.Sub(lastSnapshot) >= snapshotInterval {
					if err := EnqueueTensorGridSnapshots(500); err != nil {
						common.SysLog("TensorGrid snapshot enqueue failed: " + err.Error())
					}
					lastSnapshot = now
				}
				if delivered, failed := DeliverTensorGridCreditOutboxOnce(context.Background(), 100); failed > 0 {
					common.SysLog(fmt.Sprintf("TensorGrid credit sync: delivered=%d failed=%d", delivered, failed))
				}
				<-ticker.C
			}
		}()
	})
}

func EnqueueTensorGridSnapshots(limit int) error {
	if limit < 1 {
		limit = 500
	}
	var afterID int64
	for {
		accounts, err := model.ListTensorGridSubjectsAfter(afterID, limit)
		if err != nil {
			return err
		}
		for _, account := range accounts {
			if err := model.EnqueueTensorGridBalanceSnapshot(account.Subject); err != nil {
				return err
			}
			afterID = account.Id
		}
		if len(accounts) < limit {
			return nil
		}
	}
}

func DeliverTensorGridCreditOutboxOnce(ctx context.Context, limit int) (int, int) {
	baseURL, secret, _, _ := tensorGridSyncConfig()
	if baseURL == "" || len(secret) < 32 {
		return 0, 0
	}
	rows, err := model.GetTensorGridCreditOutboxBatch(limit)
	if err != nil {
		common.SysLog("TensorGrid outbox query failed: " + err.Error())
		return 0, 1
	}
	delivered, failed := 0, 0
	for _, row := range rows {
		if err := deliverTensorGridOutboxRow(ctx, baseURL, secret, row); err != nil {
			failed++
			_ = model.MarkTensorGridCreditOutboxFailed(row.Id, err)
			continue
		}
		delivered++
		_ = model.MarkTensorGridCreditOutboxDelivered(row.Id)
	}
	return delivered, failed
}

func deliverTensorGridOutboxRow(ctx context.Context, baseURL, secret string, row *model.TensorGridCreditOutbox) error {
	delivery, err := model.TensorGridCreditDeliveryFromOutbox(row)
	if err != nil {
		return err
	}
	payload, err := common.Marshal(delivery)
	if err != nil {
		return err
	}
	path := "/api/internal/models-gateway/v1/users/" + url.PathEscape(row.Subject)
	if row.Kind == model.TensorGridOutboxSnapshot {
		path += "/credit-snapshots"
	} else {
		path += "/credit-events"
	}
	requestURL := baseURL + path
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	bodyHash := sha256.Sum256(payload)
	canonical := timestamp + "\nPOST\n" + path + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-TensorGrid-Timestamp", timestamp)
	req.Header.Set("X-TensorGrid-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return errors.New(fmt.Sprintf("TensorGrid credit callback returned %d: %s", response.StatusCode, strings.TrimSpace(string(body))))
	}
	return nil
}
