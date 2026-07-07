package service

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newGuardTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb, mr
}

func TestAcquireDispatchGuard_ConcurrencyAcquireRelease(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()

	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", GuardrailConfig{})
	if v, _ := mr.Get("guard:conc:zhipu:glm-5.2"); v != "1" {
		t.Errorf("expected conc=1 while in-flight, got %q", v)
	}
	// R-4: the conc key must carry a TTL (heartbeat) so a crashed process self-heals.
	ttl := mr.TTL("guard:conc:zhipu:glm-5.2")
	if ttl <= 0 || ttl > time.Duration(ConcKeyTTL)*time.Second {
		t.Errorf("expected conc key TTL in (0, %ds], got %v", ConcKeyTTL, ttl)
	}
	release()
	if v, _ := mr.Get("guard:conc:zhipu:glm-5.2"); v != "0" {
		t.Errorf("expected conc=0 after release, got %q", v)
	}
}

func TestAcquireDispatchGuard_ConcurrencyReleasedOnPanic(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()

	// Simulate a dispatch body that panics. The caller's deferred release()
	// runs during panic unwind, so the counter must still be decremented.
	func() {
		defer func() { _ = recover() }()
		release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", GuardrailConfig{})
		defer release()
		panic("boom")
	}()
	if v, _ := mr.Get("guard:conc:zhipu:glm-5.2"); v != "0" {
		t.Errorf("conc leaked after panic: %q (must be 0)", v)
	}
}

func TestAcquireDispatchGuard_SetsLimitedFlagOnConcurrencyExceed(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()
	cfg := GuardrailConfig{Concurrency: 2}

	// Pre-inject conc=2 so the next acquire exceeds cap 2.
	mr.Set("guard:conc:zhipu:glm-5.2", "2")
	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	if !mr.Exists("guard:limited:zhipu:glm-5.2:conc") {
		t.Errorf("expected guard:limited:...:conc flag set when conc exceeds cap")
	}
	release()
}

func TestAcquireDispatchGuard_RPMIncrementsAndFlagsExceed(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()
	cfg := GuardrailConfig{RPM: 1}

	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	minute := currentMinuteKeySuffix()
	if v, _ := mr.Get("guard:rpm:zhipu:glm-5.2:" + minute); v != "1" {
		t.Errorf("expected rpm=1 after first acquire, got %q (minute key=%s)", v, minute)
	}
	release()

	// Second acquire within the same minute exceeds RPM=1 -> flag set.
	release2 := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	if !mr.Exists("guard:limited:zhipu:glm-5.2:rpm") {
		t.Errorf("expected guard:limited:...:rpm flag set when RPM exceeds")
	}
	release2()
}

func TestAcquireDispatchGuard_DisabledWhenConfigZero(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()

	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", GuardrailConfig{})
	release()
	if mr.Exists("guard:limited:zhipu:glm-5.2:conc") || mr.Exists("guard:limited:zhipu:glm-5.2:rpm") {
		t.Errorf("no limited flags expected when config is zero")
	}
}

func TestAcquireDispatchGuard_NilRedisNoop(t *testing.T) {
	// nil rdb (Community without Redis configured) must not panic.
	ctx := context.Background()
	release := AcquireDispatchGuard(ctx, nil, "zhipu", "glm-5.2", GuardrailConfig{Concurrency: 1, RPM: 1})
	release()
}

func TestAcquireDispatchGuard_OnExceedFiresWithDim(t *testing.T) {
	rdb, mr := newGuardTestRedis(t)
	ctx := context.Background()

	// Pre-inject conc=2 so this acquire (cap 2) is the over-cap 3rd.
	mr.Set("guard:conc:zhipu:glm-5.2", "2")
	var calls []string
	cfg := GuardrailConfig{
		Concurrency: 2,
		OnExceed:    func(dim string) { calls = append(calls, dim) },
	}
	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	release()

	found := false
	for _, c := range calls {
		if c == "conc" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected OnExceed('conc') to fire on concurrency exceed; calls=%v", calls)
	}
}

func TestAcquireDispatchGuard_OnExceedFiresOnRPM(t *testing.T) {
	rdb, _ := newGuardTestRedis(t)
	ctx := context.Background()

	cfg := GuardrailConfig{
		RPM:      1,
		OnExceed: func(dim string) { /* will assert via a fresh callback below */ },
	}
	// First acquire in the minute is at RPM=1 (== cap, not over). Release it.
	r1 := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	r1()

	// Second acquire in the same minute pushes to 2 (>1 cap) -> OnExceed fires.
	var calls []string
	cfg2 := GuardrailConfig{
		RPM:      1,
		OnExceed: func(dim string) { calls = append(calls, dim) },
	}
	r2 := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg2)
	r2()

	found := false
	for _, c := range calls {
		if c == "rpm" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected OnExceed('rpm') to fire on RPM exceed; calls=%v", calls)
	}
}

func TestAcquireDispatchGuard_OnExceedNotCalledWhenWithinCap(t *testing.T) {
	rdb, _ := newGuardTestRedis(t)
	ctx := context.Background()

	calls := []string{}
	cfg := GuardrailConfig{
		Concurrency: 10,
		RPM:         100,
		OnExceed:    func(dim string) { calls = append(calls, dim) },
	}
	release := AcquireDispatchGuard(ctx, rdb, "zhipu", "glm-5.2", cfg)
	release()
	if len(calls) != 0 {
		t.Errorf("OnExceed must not fire when within cap; calls=%v", calls)
	}
}
