package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/crosslink/internal/provider"
	"github.com/redis/go-redis/v9"
)

// HealthScorePenaltyFactor controls how aggressively error rate de-weights a
// provider. base = max(0, 1 - errorRate * penaltyFactor). With 2.0, a 30% error
// rate yields base 0.4; 50%+ clamps to 0.
const HealthScorePenaltyFactor = 2.0

// HalfOpenProbeFloor is the score assigned to a half-open circuit: only a small
// fraction of traffic should probe a recovering provider.
const HalfOpenProbeFloor = 0.05

// HealthScore computes a health score ∈ [0, 1] for a (provider, model) from:
//   - HealthTracker circuit state (open → 0, half-open → HalfOpenProbeFloor)
//   - P3a guard:limited flags (conc / rpm → 0)
//   - error rate (guard:errs / guard:total in the current minute) → soft de-weight
//
// P4 (v5.1 R-1): the denominator is guard:total (written on EVERY dispatch via
// RecordDispatchOutcome), NOT guard:rpm — so soft de-weight covers ALL providers,
// not just those with RPM guardrail config. Latency branch deferred.
//
// nil rdb or nil health → score 1.0 (fail-open: no observability infra).
func HealthScore(ctx context.Context, rdb *redis.Client, health *provider.HealthTracker, providerName, model string) float64 {
	if rdb == nil || health == nil {
		return 1.0
	}

	// Circuit state takes precedence (hard states override soft base).
	switch st := health.CircuitState(providerName, model); st {
	case provider.CircuitOpen:
		return 0
	case provider.CircuitHalfOpen:
		return HalfOpenProbeFloor
	}

	// P3a guard:limited flags (conc/rpm) → hard zero.
	if limited, _ := rdb.Exists(ctx,
		fmt.Sprintf("guard:limited:%s:%s:conc", providerName, model),
		fmt.Sprintf("guard:limited:%s:%s:rpm", providerName, model),
	).Result(); limited > 0 {
		return 0
	}

	// Soft de-weight by error rate over the current minute bucket.
	// Denominator is guard:total (R-1): universal across all providers.
	minute := currentMinuteKeySuffix()
	totalKey := fmt.Sprintf("guard:total:%s:%s:%s", providerName, model, minute)
	errsKey := fmt.Sprintf("guard:errs:%s:%s:%s", providerName, model, minute)
	totalStr, _ := rdb.Get(ctx, totalKey).Result()
	errsStr, _ := rdb.Get(ctx, errsKey).Result()
	total, _ := strconv.ParseInt(totalStr, 10, 64)
	errs, _ := strconv.ParseInt(errsStr, 10, 64)
	if total <= 0 {
		return 1.0 // no traffic recorded this minute → assume healthy
	}
	errorRate := float64(errs) / float64(total)
	base := 1.0 - errorRate*HealthScorePenaltyFactor
	if base < 0 {
		base = 0
	}
	return base
}

// RecordDispatchOutcome records a dispatch outcome for (provider, model) in the
// current minute bucket: increments guard:total ALWAYS, and guard:errs if err
// != nil. Fed from every dispatch callFn (success AND failure). The guard:total
// counter is the universal denominator for HealthScore's soft de-weight (R-1),
// so soft de-weight covers all providers regardless of guardrail config.
//
// nil rdb is a no-op. Sync (1-2 Redis INCR per dispatch — cheap relative to the
// upstream HTTP call the dispatch itself makes).
func RecordDispatchOutcome(ctx context.Context, rdb *redis.Client, providerName, model string, err error) {
	if rdb == nil {
		return
	}
	minute := currentMinuteKeySuffix()
	totalKey := fmt.Sprintf("guard:total:%s:%s:%s", providerName, model, minute)
	if e := rdb.Incr(ctx, totalKey).Err(); e != nil {
		slog.Warn("record dispatch total incr failed", "key", totalKey, "error", e)
	} else {
		rdb.Expire(ctx, totalKey, 2*time.Minute) // TTL on first write
	}
	if err != nil {
		errsKey := fmt.Sprintf("guard:errs:%s:%s:%s", providerName, model, minute)
		if e := rdb.Incr(ctx, errsKey).Err(); e != nil {
			slog.Warn("record dispatch errs incr failed", "key", errsKey, "error", e)
		} else {
			rdb.Expire(ctx, errsKey, 2*time.Minute)
		}
	}
}
