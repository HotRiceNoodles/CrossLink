package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestWithRetry_NoRetries(t *testing.T) {
	calls := 0
	rr := WithRetry(context.Background(), RetryConfig{NumRetries: 0}, nil, func(ctx context.Context) error {
		calls++
		return fmt.Errorf("fail")
	})
	if rr.Err == nil {
		t.Error("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestWithRetry_SuccessOnThirdAttempt(t *testing.T) {
	calls := 0
	rr := WithRetry(context.Background(), RetryConfig{NumRetries: 3}, nil, func(ctx context.Context) error {
		calls++
		if calls < 3 {
			return &ProviderError{StatusCode: 500, ErrorType: ErrorServer, Message: "fail"}
		}
		return nil
	})
	if rr.Err != nil {
		t.Errorf("expected nil error, got %v", rr.Err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestWithRetry_NonRetryableStopsImmediately(t *testing.T) {
	calls := 0
	rr := WithRetry(context.Background(), RetryConfig{NumRetries: 5}, nil, func(ctx context.Context) error {
		calls++
		return &ProviderError{StatusCode: 401, ErrorType: ErrorAuth, Message: "unauthorized"}
	})
	if rr.Err == nil {
		t.Error("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call for non-retryable, got %d", calls)
	}
}

func TestWithRetry_AllRetriesExhausted(t *testing.T) {
	calls := 0
	rr := WithRetry(context.Background(), RetryConfig{NumRetries: 2}, nil, func(ctx context.Context) error {
		calls++
		return &ProviderError{StatusCode: 500, ErrorType: ErrorServer, Message: "fail"}
	})
	if rr.Err == nil {
		t.Error("expected error")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (1 + 2 retries), got %d", calls)
	}
}

func TestWithRetry_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	rr := WithRetry(ctx, RetryConfig{NumRetries: 3}, nil, func(ctx context.Context) error {
		calls++
		return &ProviderError{StatusCode: 500, ErrorType: ErrorServer, Message: "fail"}
	})
	if rr.Err == nil {
		t.Error("expected error")
	}
	if calls != 0 {
		t.Errorf("expected 0 calls with cancelled context, got %d", calls)
	}
}

func TestWithRetry_SuccessFirstTry(t *testing.T) {
	calls := 0
	rr := WithRetry(context.Background(), RetryConfig{NumRetries: 3}, nil, func(ctx context.Context) error {
		calls++
		return nil
	})
	if rr.Err != nil {
		t.Errorf("expected nil error, got %v", rr.Err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestParseRetryConfig(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected RetryConfig
	}{
		{"empty", ``, RetryConfig{NumRetries: 0}},
		{"valid", `{"num_retries": 3}`, RetryConfig{NumRetries: 3}},
		{"negative", `{"num_retries": -1}`, RetryConfig{NumRetries: 0}},
		{"zero", `{"num_retries": 0}`, RetryConfig{NumRetries: 0}},
		{"with_backoff", `{"num_retries": 2, "backoff_type": "fixed", "initial_ms": 500}`, RetryConfig{NumRetries: 2, BackoffType: "fixed", InitialMs: 500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw json.RawMessage
			if tt.input != "" {
				raw = json.RawMessage(tt.input)
			}
			got := ParseRetryConfig(raw)
			if got != tt.expected {
				t.Errorf("ParseRetryConfig(%q) = %+v, want %+v", tt.input, got, tt.expected)
			}
		})
	}
}

func TestBackoffDuration_RateLimitDoubles(t *testing.T) {
	err := &ProviderError{ErrorType: ErrorRateLimit, Message: "rate limited"}
	d := backoffDuration(0, err, RetryConfig{})
	if d < 2*time.Second {
		t.Errorf("rate limit backoff = %v, want >= 2s", d)
	}
}

func TestBackoffDuration_RespectsRetryAfter(t *testing.T) {
	pe := &ProviderError{
		ErrorType:  ErrorRateLimit,
		RetryAfter: 10 * time.Second,
	}
	d := backoffDuration(0, pe, RetryConfig{})
	if d != 10*time.Second {
		t.Errorf("expected exactly RetryAfter (10s), got %v", d)
	}
}

func TestBackoffDuration_RetryAfterOnAllAttempts(t *testing.T) {
	pe := &ProviderError{
		ErrorType:  ErrorRateLimit,
		RetryAfter: 10 * time.Second,
	}
	d := backoffDuration(3, pe, RetryConfig{})
	if d < 10*time.Second {
		t.Errorf("expected RetryAfter to be respected on attempt > 0, got %v", d)
	}
}

func TestBackoffDuration_WrappedError(t *testing.T) {
	inner := &ProviderError{
		ErrorType:  ErrorRateLimit,
		RetryAfter: 10 * time.Second,
	}
	wrapped := fmt.Errorf("call failed: %w", inner)
	d := backoffDuration(0, wrapped, RetryConfig{})
	if d < 10*time.Second {
		t.Errorf("expected RetryAfter from wrapped error, got %v", d)
	}
}

func TestBackoffDuration_MaxCap(t *testing.T) {
	err := &ProviderError{ErrorType: ErrorServer, Message: "fail"}
	d := backoffDuration(20, err, RetryConfig{})
	if d > 60*time.Second {
		t.Errorf("backoff %v exceeds max 60s", d)
	}
}

func TestBackoffDuration_FixedStrategy(t *testing.T) {
	err := &ProviderError{ErrorType: ErrorServer, Message: "fail"}
	cfg := RetryConfig{BackoffType: "fixed", InitialMs: 500}
	for attempt := 0; attempt < 4; attempt++ {
		d := backoffDuration(attempt, err, cfg)
		if d < 500*time.Millisecond || d > 1*time.Second {
			t.Errorf("fixed backoff attempt %d = %v, want ~500ms+ jitter", attempt, d)
		}
	}
}

func TestBackoffDuration_LinearStrategy(t *testing.T) {
	err := &ProviderError{ErrorType: ErrorServer, Message: "fail"}
	cfg := RetryConfig{BackoffType: "linear", InitialMs: 1000}
	d0 := backoffDuration(0, err, cfg)
	d1 := backoffDuration(1, err, cfg)
	d2 := backoffDuration(2, err, cfg)
	if d0 >= d1 || d1 >= d2 {
		t.Errorf("linear should increase: attempt0=%v, attempt1=%v, attempt2=%v", d0, d1, d2)
	}
}

func TestBackoffDuration_CustomMaxBackoff(t *testing.T) {
	err := &ProviderError{ErrorType: ErrorServer, Message: "fail"}
	cfg := RetryConfig{MaxBackoffMs: 3000}
	d := backoffDuration(20, err, cfg)
	if d > 3*time.Second {
		t.Errorf("backoff %v exceeds custom max 3s", d)
	}
}
