package provider

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"
)

type RetryConfig struct {
	NumRetries   int    `json:"num_retries"`
	BackoffType  string `json:"backoff_type"`   // "exponential" (default) | "fixed" | "linear"
	InitialMs    int    `json:"initial_ms"`     // default 1000
	MaxBackoffMs int    `json:"max_backoff_ms"` // default 60000
}

func ParseRetryConfig(extraConfig json.RawMessage) RetryConfig {
	if len(extraConfig) == 0 {
		return RetryConfig{}
	}
	var cfg struct {
		NumRetries   int    `json:"num_retries"`
		BackoffType  string `json:"backoff_type"`
		InitialMs    int    `json:"initial_ms"`
		MaxBackoffMs int    `json:"max_backoff_ms"`
	}
	if json.Unmarshal(extraConfig, &cfg) != nil {
		return RetryConfig{}
	}
	if cfg.NumRetries < 0 {
		cfg.NumRetries = 0
	}
	if cfg.NumRetries > 20 {
		cfg.NumRetries = 20
	}
	if cfg.InitialMs < 0 {
		cfg.InitialMs = 0
	}
	if cfg.InitialMs > 60000 {
		cfg.InitialMs = 60000
	}
	if cfg.MaxBackoffMs < 0 {
		cfg.MaxBackoffMs = 0
	}
	if cfg.MaxBackoffMs > 300000 {
		cfg.MaxBackoffMs = 300000
	}
	return RetryConfig{
		NumRetries:   cfg.NumRetries,
		BackoffType:  cfg.BackoffType,
		InitialMs:    cfg.InitialMs,
		MaxBackoffMs: cfg.MaxBackoffMs,
	}
}

type RetryResult struct {
	Err         error
	Attempts    int
	RetriesUsed int
}

// retryAttemptKey is the context key for the current retry attempt number.
type retryAttemptKey struct{}

// AttemptFromContext returns the 1-based attempt number from the context.
func AttemptFromContext(ctx context.Context) int {
	v, _ := ctx.Value(retryAttemptKey{}).(int)
	return v
}

// WithRetry calls `call` up to 1 + cfg.NumRetries times with configurable backoff.
// Non-retryable errors and exhausted retry budget return immediately.
// The callback receives a context enriched with the current attempt number.
func WithRetry(ctx context.Context, cfg RetryConfig, budget *RetryBudget, call func(ctx context.Context) error) RetryResult {
	if cfg.NumRetries <= 0 {
		attemptCtx := context.WithValue(ctx, retryAttemptKey{}, 1)
		err := call(attemptCtx)
		if err != nil {
			return RetryResult{Err: err, Attempts: 1, RetriesUsed: 0}
		}
		return RetryResult{Attempts: 1, RetriesUsed: 0}
	}

	var lastErr error
	for attempt := 0; attempt <= cfg.NumRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return RetryResult{Err: err, Attempts: attempt, RetriesUsed: attempt}
		}

		attemptCtx := context.WithValue(ctx, retryAttemptKey{}, attempt+1)
		lastErr = call(attemptCtx)
		if lastErr == nil {
			return RetryResult{Attempts: attempt + 1, RetriesUsed: attempt}
		}

		// Don't retry non-retryable errors
		if !IsRetryableError(lastErr) {
			return RetryResult{Err: lastErr, Attempts: attempt + 1, RetriesUsed: attempt}
		}

		// Don't sleep after the last attempt
		if attempt == cfg.NumRetries {
			break
		}

		// Check retry budget
		if budget != nil && !budget.Allow(ctx) {
			return RetryResult{Err: lastErr, Attempts: attempt + 1, RetriesUsed: attempt}
		}

		backoff := backoffDuration(attempt, lastErr, cfg)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return RetryResult{Err: ctx.Err(), Attempts: attempt + 1, RetriesUsed: attempt}
		case <-timer.C:
		}
	}
	return RetryResult{Err: lastErr, Attempts: cfg.NumRetries + 1, RetriesUsed: cfg.NumRetries}
}

func backoffDuration(attempt int, err error, cfg RetryConfig) time.Duration {
	initial := time.Duration(cfg.InitialMs) * time.Millisecond
	if initial == 0 {
		initial = time.Second
	}
	maxBackoff := time.Duration(cfg.MaxBackoffMs) * time.Millisecond
	if maxBackoff == 0 {
		maxBackoff = 60 * time.Second
	}

	var base time.Duration
	switch cfg.BackoffType {
	case "fixed":
		base = initial
	case "linear":
		base = initial * time.Duration(attempt+1)
	default: // exponential
		base = initial << uint(min(attempt, 6))
	}

	// Rate-limit errors get 2x backoff
	if ClassifyError(err) == ErrorRateLimit {
		base *= 2
	}

	// Respect Retry-After header - capped by maxBackoff
	var pe *ProviderError
	if err != nil {
		errors.As(err, &pe)
	}
	if pe != nil && pe.RetryAfter > 0 {
		if pe.RetryAfter > maxBackoff {
			return maxBackoff
		}
		return pe.RetryAfter
	}

	jitter := time.Duration(rand.IntN(500)) * time.Millisecond
	d := base + jitter
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}
