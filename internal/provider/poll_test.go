package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P1 acceptance: PollUntil bridges sync callers to async upstreams —
// done/timeout/cancel/error semantics, backoff behavior, and the
// retryability of ErrPollTimeout.

func TestPollUntil_Done(t *testing.T) {
	t.Parallel()
	calls := 0
	err := PollUntil(context.Background(), PollConfig{
		Timeout: 5 * time.Second, Interval: 10 * time.Millisecond, MaxInterval: 50 * time.Millisecond,
	}, func(ctx context.Context) (bool, error) {
		calls++
		return calls >= 3, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "third poll should report done")
}

func TestPollUntil_Timeout(t *testing.T) {
	t.Parallel()
	calls := 0
	start := time.Now()
	err := PollUntil(context.Background(), PollConfig{
		Timeout: 200 * time.Millisecond, Interval: 20 * time.Millisecond, MaxInterval: 50 * time.Millisecond,
	}, func(ctx context.Context) (bool, error) {
		calls++
		return false, nil // never done
	})
	require.ErrorIs(t, err, ErrPollTimeout)
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond, "must wait the full budget")
	assert.Less(t, time.Since(start), 500*time.Millisecond, "must not run far past the budget")
	assert.Greater(t, calls, 2, "must have polled multiple times")
	assert.False(t, IsRetryableError(err) == false, "timeout must be retryable (fallback eligible)")
}

func TestPollUntil_CheckError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("auth failed")
	calls := 0
	err := PollUntil(context.Background(), PollConfig{
		Timeout: 5 * time.Second, Interval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond,
	}, func(ctx context.Context) (bool, error) {
		calls++
		return false, sentinel
	})
	require.ErrorIs(t, err, sentinel, "check errors surface immediately")
	assert.Equal(t, 1, calls, "no retry after an error return")
}

func TestPollUntil_CtxCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	err := PollUntil(ctx, PollConfig{
		Timeout: 5 * time.Second, Interval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond,
	}, func(ctx context.Context) (bool, error) {
		return false, nil
	})
	require.ErrorIs(t, err, context.Canceled, "caller cancellation surfaces as ctx error, not ErrPollTimeout")
}

func TestPollUntil_Backoff(t *testing.T) {
	t.Parallel()
	var sleeps []time.Duration
	// Instrument: track the intervals between check invocations.
	last := time.Now()
	err := PollUntil(context.Background(), PollConfig{
		Timeout: 2 * time.Second, Interval: 30 * time.Millisecond, MaxInterval: 200 * time.Millisecond,
	}, func(ctx context.Context) (bool, error) {
		now := time.Now()
		sleeps = append(sleeps, now.Sub(last))
		last = now
		return len(sleeps) >= 5, nil
	})
	require.NoError(t, err)
	require.Len(t, sleeps, 5)
	// First sleep ≈ 30ms; subsequent ones grow (backoff) but ≤ 200ms.
	assert.InDelta(t, 30*time.Millisecond, float64(sleeps[0]), float64(15*time.Millisecond))
	for i := 1; i < len(sleeps); i++ {
		assert.LessOrEqual(t, sleeps[i], 250*time.Millisecond, "backoff capped at MaxInterval")
	}
}

func TestPollConfigFromExtra(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		extra string
		want  PollConfig
	}{
		{"empty → defaults", ``, PollConfig{Timeout: 120 * time.Second, Interval: 3 * time.Second, MaxInterval: 5 * time.Second}},
		{"valid values", `{"poll_timeout_seconds":300,"poll_interval_ms":2000,"poll_max_interval_ms":8000}`,
			PollConfig{Timeout: 300 * time.Second, Interval: 2 * time.Second, MaxInterval: 8 * time.Second}},
		{"timeout too low → default", `{"poll_timeout_seconds":5}`, PollConfig{Timeout: 120 * time.Second, Interval: 3 * time.Second, MaxInterval: 5 * time.Second}},
		{"timeout too high → default", `{"poll_timeout_seconds":7200}`, PollConfig{Timeout: 120 * time.Second, Interval: 3 * time.Second, MaxInterval: 5 * time.Second}},
		{"interval too low → default", `{"poll_interval_ms":100}`, PollConfig{Timeout: 120 * time.Second, Interval: 3 * time.Second, MaxInterval: 5 * time.Second}},
		{"max < interval → degenerate", `{"poll_interval_ms":5000,"poll_max_interval_ms":1000}`,
			PollConfig{Timeout: 120 * time.Second, Interval: 5 * time.Second, MaxInterval: 5 * time.Second}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := PollConfigFromExtra([]byte(tc.extra))
			assert.Equal(t, tc.want, got)
		})
	}
}
