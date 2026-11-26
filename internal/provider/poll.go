package provider

import (
	"context"
	"time"
)

// ErrPollTimeout is returned by PollUntil when the deadline expires before
// the check reports done. It is a RETRYABLE error (ErrorTimeout class):
// the FallbackEngine treats it as "this candidate took too long" and moves
// to the next one — typically a synchronous upstream that can serve fast.
// It must be a *ProviderError so ClassifyError→IsRetryableError recognize
// it as ErrorTimeout (a bare errors.New would classify as "" = non-retryable
// and the fallback would never fire).
var ErrPollTimeout = &ProviderError{
	ErrorType: ErrorTimeout,
	Message:   "poll timeout: async upstream did not complete within the configured budget",
}

// PollConfig bounds a PollUntil loop. All three values are required;
// ValidateCommonExtraConfig enforces their legal ranges at write time
// (图片方案 P1, docs/design/image-model-integration.md §3.5).
type PollConfig struct {
	Timeout     time.Duration // total budget
	Interval    time.Duration // initial sleep between polls
	MaxInterval time.Duration // backoff cap (≥ Interval)
}

// PollUntil bridges a synchronous caller to an asynchronous (task-based)
// upstream: run check every Interval (backing off toward MaxInterval) until
// it returns done=true, the deadline expires, or ctx is cancelled.
//
// Error semantics:
//   - check returning a non-nil error → PollUntil returns it immediately
//     (the adapter decides: network errors may be retried inside check;
//     auth errors fail fast)
//   - deadline expiry → ErrPollTimeout (retryable, triggers fallback)
//   - ctx cancellation → the raw ctx error (caller's cancellation, not a
//     provider verdict — never negative-cached)
//
// Backoff: the first poll runs after Interval; each subsequent sleep grows
// by 1.5× up to MaxInterval. The first check runs AFTER the initial sleep
// because most task APIs need a moment before reporting anything useful.
func PollUntil(ctx context.Context, cfg PollConfig, check func(ctx context.Context) (bool, error)) error {
	deadline := time.Now().Add(cfg.Timeout)
	interval := cfg.Interval

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		done, err := check(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}

		if !time.Now().Before(deadline) {
			return ErrPollTimeout
		}

		// Backoff toward MaxInterval.
		interval = time.Duration(float64(interval) * 1.5)
		if interval > cfg.MaxInterval {
			interval = cfg.MaxInterval
		}
		if wouldExceed := time.Until(deadline); interval > wouldExceed {
			interval = wouldExceed // don't sleep past the deadline
		}
	}
}
