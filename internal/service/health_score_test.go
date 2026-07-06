package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/provider"
	"github.com/redis/go-redis/v9"
)

func newScoreTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb, mr
}

func TestHealthScore_HealthyWhenNothingWrong(t *testing.T) {
	rdb, _ := newScoreTestRedis(t)
	h := provider.NewHealthTracker()
	score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2")
	if score != 1.0 {
		t.Errorf("expected 1.0 for healthy provider, got %f", score)
	}
}

func TestHealthScore_ZeroWhenCircuitOpen(t *testing.T) {
	rdb, _ := newScoreTestRedis(t)
	h := provider.NewHealthTrackerWithConfig(3, 60*time.Second)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-5.2", 0)
	}
	if score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2"); score != 0 {
		t.Errorf("expected 0 when circuit open, got %f", score)
	}
}

func TestHealthScore_HalfOpenReturnsProbeFloor(t *testing.T) {
	rdb, _ := newScoreTestRedis(t)
	h := provider.NewHealthTrackerWithConfig(3, 30*time.Millisecond)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-5.2", 0)
	}
	time.Sleep(40 * time.Millisecond) // cooldown expired -> half-open
	if score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2"); score != 0.05 {
		t.Errorf("expected 0.05 (half-open probe floor), got %f", score)
	}
}

func TestHealthScore_ZeroWhenGuardLimitedFlagSet(t *testing.T) {
	rdb, mr := newScoreTestRedis(t)
	h := provider.NewHealthTracker()
	// simulate P3a setting the conc-limited flag
	mr.Set("guard:limited:zhipu:glm-5.2:conc", "1")
	if score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2"); score != 0 {
		t.Errorf("expected 0 when guard:limited flag set, got %f", score)
	}
}

func TestHealthScore_SoftDeweightByErrorRate(t *testing.T) {
	rdb, mr := newScoreTestRedis(t)
	h := provider.NewHealthTracker()
	// Simulate a minute bucket with 10 total dispatches, 3 errors -> errorRate 0.3.
	// base = max(0, 1 - 0.3*2.0) = 0.4. Denominator is guard:total (R-1).
	minute := currentMinuteKeySuffix()
	mr.Set("guard:total:zhipu:glm-5.2:"+minute, "10")
	mr.Set("guard:errs:zhipu:glm-5.2:"+minute, "3")
	score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2")
	if score < 0.39 || score > 0.41 {
		t.Errorf("expected ~0.4 from 30%% error rate, got %f", score)
	}
}

func TestHealthScore_ErrorRateClampedAtZero(t *testing.T) {
	rdb, mr := newScoreTestRedis(t)
	h := provider.NewHealthTracker()
	// 80% error rate -> 1 - 0.8*2 = -0.6 -> clamp to 0. Denominator guard:total (R-1).
	minute := currentMinuteKeySuffix()
	mr.Set("guard:total:zhipu:glm-5.2:"+minute, "10")
	mr.Set("guard:errs:zhipu:glm-5.2:"+minute, "8")
	if score := HealthScore(context.Background(), rdb, h, "zhipu", "glm-5.2"); score != 0 {
		t.Errorf("expected 0 (clamped) for 80%% error rate, got %f", score)
	}
}

func TestRecordDispatchOutcome_IncrementsTotalAndErrs(t *testing.T) {
	rdb, mr := newScoreTestRedis(t)
	ctx := context.Background()
	minute := currentMinuteKeySuffix()
	// Success: total++ only.
	RecordDispatchOutcome(ctx, rdb, "zhipu", "glm-5.2", nil)
	// Failure: total++ AND errs++.
	RecordDispatchOutcome(ctx, rdb, "zhipu", "glm-5.2", fmt.Errorf("boom"))
	RecordDispatchOutcome(ctx, rdb, "zhipu", "glm-5.2", fmt.Errorf("boom2"))

	total, _ := mr.Get("guard:total:zhipu:glm-5.2:" + minute)
	errs, _ := mr.Get("guard:errs:zhipu:glm-5.2:" + minute)
	if total != "3" {
		t.Errorf("expected total=3 (all dispatches), got %q", total)
	}
	if errs != "2" {
		t.Errorf("expected errs=2 (failures only), got %q", errs)
	}
}
