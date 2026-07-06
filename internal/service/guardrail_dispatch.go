package service

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// GuardrailConfig is the per-(provider,model) guardrail limits.
// Zero values disable that dimension. Populated from provider_model ExtraConfig
// (json fields "concurrency" and "rpm").
type GuardrailConfig struct {
	Concurrency int
	RPM         int
	// OnExceed is invoked (with "conc" or "rpm") when THIS acquire pushes the
	// counter over its configured cap. nil = no callback (default — Community
	// call sites leave this unset). The commercial overlay sets it to feed a
	// multi-channel alert service (B1). The callback MUST be cheap (the alert
	// service does its own async dispatch + cooldown); it fires per over-cap
	// request, so dedup is the alert service's responsibility.
	OnExceed func(dim string)
}

// AcquireDispatchGuard records per-(provider,model) concurrency + RPM counters
// to Redis and sets guard:limited flags when over cap. It returns a release
// function that MUST be deferred by the caller (typically `defer release()` in
// the dispatch callFn closure).
//
// COUNT-ONLY (P3a): it does NOT block dispatch. Enforcement (skipping over-cap
// routes) is deferred to P4's health-score, which reads guard:limited to set
// effective weight 0. This avoids entangling with FallbackEngine's error
// classifier (a capacity exceedance is not a provider failure).
//
// Concurrency: INCR on acquire, DECR on release. The release func does NOT
// recover — panic propagates to gin's Recovery middleware — but the DECR still
// runs because deferred funcs execute during panic unwind. This fixes the
// leak pattern in the existing activeTracker.Incr/Decr (handler/openai.go) which
// lacks defer protection.
//
// RPM: INCR a minute-bucket counter (TTL 120s) per dispatch.
//
// nil rdb is a no-op (Community without Redis configured).
func AcquireDispatchGuard(ctx context.Context, rdb *redis.Client, providerName, model string, cfg GuardrailConfig) (release func()) {
	if rdb == nil {
		return func() {}
	}

	concKey := fmt.Sprintf("guard:conc:%s:%s", providerName, model)
	minute := currentMinuteKeySuffix()
	rpmKey := fmt.Sprintf("guard:rpm:%s:%s:%s", providerName, model, minute)

	// Concurrency: acquire.
	concNow, err := rdb.Incr(ctx, concKey).Result()
	if err != nil {
		slog.Warn("guardrail conc incr failed", "key", concKey, "error", err)
	} else if cfg.Concurrency > 0 && concNow > int64(cfg.Concurrency) {
		setLimited(ctx, rdb, providerName, model, "conc", 15)
		if cfg.OnExceed != nil {
			cfg.OnExceed("conc")
		}
	}

	// RPM: increment minute bucket (TTL on first write).
	if n, err := incrExpireGuard(rdb, ctx, rpmKey, 120); err != nil {
		slog.Warn("guardrail rpm incr failed", "key", rpmKey, "error", err)
	} else if cfg.RPM > 0 && n > int64(cfg.RPM) {
		// TTL until the current minute boundary rolls over (+1s buffer).
		ttl := 60 - time.Now().Second() + 1
		if ttl < 1 {
			ttl = 1
		}
		setLimited(ctx, rdb, providerName, model, "rpm", ttl)
		if cfg.OnExceed != nil {
			cfg.OnExceed("rpm")
		}
	}

	return func() {
		// DECR uses a background context: the caller's request context may be
		// cancelled by the time release runs (response written / panic).
		if err := rdb.Decr(context.Background(), concKey).Err(); err != nil {
			slog.Warn("guardrail conc decr failed", "key", concKey, "error", err)
		}
	}
}

// currentMinuteKeySuffix returns the unix-minute as a string, used in Redis keys.
// Lower-cased to match naming conventions; exposed for tests.
func currentMinuteKeySuffix() string {
	return strconv.FormatInt(time.Now().Unix()/60, 10)
}

// incrExpireGuard INCRs and sets TTL only on first increment (mirrors the
// incrExpireScript Lua pattern in middleware/ratelimit.go).
func incrExpireGuard(rdb *redis.Client, ctx context.Context, key string, ttlSec int) (int64, error) {
	n, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := rdb.Expire(ctx, key, time.Duration(ttlSec)*time.Second).Err(); err != nil {
			slog.Warn("guardrail rpm expire failed", "key", key, "error", err)
		}
	}
	return n, nil
}

func setLimited(ctx context.Context, rdb *redis.Client, providerName, model, dim string, ttlSec int) {
	key := fmt.Sprintf("guard:limited:%s:%s:%s", providerName, model, dim)
	if err := rdb.Set(ctx, key, "1", time.Duration(ttlSec)*time.Second).Err(); err != nil {
		slog.Warn("guardrail set limited flag failed", "key", key, "error", err)
	}
}
