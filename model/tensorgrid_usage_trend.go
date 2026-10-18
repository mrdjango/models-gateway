package model

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

type TensorGridUsageTrendPoint struct {
	BucketStart int64 `json:"bucket_start"`
	TensorGridUsageTotals
}

type TensorGridUsageModelTotals struct {
	Model string `json:"model"`
	TensorGridUsageTotals
}

// TensorGridUsageTrend aggregates a user's usage logs into time buckets and
// per-model totals in one pass, so callers never page through raw logs to
// draw a chart. Points holds only buckets that have usage.
type TensorGridUsageTrend struct {
	Bucket         string                       `json:"bucket"`
	FirstCreatedAt int64                        `json:"first_created_at"`
	Totals         TensorGridUsageTotals        `json:"totals"`
	Points         []TensorGridUsageTrendPoint  `json:"points"`
	ByModel        []TensorGridUsageModelTotals `json:"by_model"`
}

// TensorGridUsageBucketStart floors a log timestamp to the start of its hour,
// day, or month on the wall clock of loc.
func TensorGridUsageBucketStart(createdAt int64, bucket string, loc *time.Location) int64 {
	local := time.Unix(createdAt, 0).In(loc)
	switch bucket {
	case "hour":
		// Floor with the offset in force at this instant, so the repeated hour
		// of a DST fall-back stays two distinct buckets.
		_, offset := local.Zone()
		wall := createdAt + int64(offset)
		return wall - ((wall%3600)+3600)%3600 - int64(offset)
	case "month":
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).Unix()
	default:
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).Unix()
	}
}

func GetTensorGridUserUsageTrend(
	account *TensorGridAccount, filter TensorGridUsageFilter, bucket string, loc *time.Location,
) (*TensorGridUsageTrend, error) {
	base, err := tensorGridUsageQuery(account, filter)
	if err != nil {
		return nil, err
	}
	trend := &TensorGridUsageTrend{Bucket: bucket, Totals: newTensorGridUsageTotals()}
	points := map[int64]*TensorGridUsageTrendPoint{}
	models := map[string]*TensorGridUsageModelTotals{}
	err = scanTensorGridUsage(base, account, func(log *Log, charge tensorGridUsageCharge) {
		trend.Totals.add(charge)
		if log.CreatedAt > 0 && (trend.FirstCreatedAt == 0 || log.CreatedAt < trend.FirstCreatedAt) {
			trend.FirstCreatedAt = log.CreatedAt
		}
		start := TensorGridUsageBucketStart(log.CreatedAt, bucket, loc)
		point, ok := points[start]
		if !ok {
			point = &TensorGridUsageTrendPoint{BucketStart: start, TensorGridUsageTotals: newTensorGridUsageTotals()}
			points[start] = point
		}
		point.add(charge)
		name := strings.TrimSpace(log.ModelName)
		modelTotals, ok := models[name]
		if !ok {
			modelTotals = &TensorGridUsageModelTotals{Model: name, TensorGridUsageTotals: newTensorGridUsageTotals()}
			models[name] = modelTotals
		}
		modelTotals.add(charge)
	})
	if err != nil {
		return nil, err
	}
	trend.Points = make([]TensorGridUsageTrendPoint, 0, len(points))
	for _, point := range points {
		trend.Points = append(trend.Points, *point)
	}
	slices.SortFunc(trend.Points, func(a, b TensorGridUsageTrendPoint) int {
		return cmp.Compare(a.BucketStart, b.BucketStart)
	})
	trend.ByModel = make([]TensorGridUsageModelTotals, 0, len(models))
	for _, modelTotals := range models {
		trend.ByModel = append(trend.ByModel, *modelTotals)
	}
	slices.SortFunc(trend.ByModel, func(a, b TensorGridUsageModelTotals) int {
		return cmp.Compare(a.Model, b.Model)
	})
	return trend, nil
}
