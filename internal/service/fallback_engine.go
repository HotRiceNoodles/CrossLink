package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// RouteResolver resolves model names to route results.
type RouteResolver interface {
	Resolve(ctx context.Context, modelName string, orgID int64) ([]*router.RouteResult, error)
}

// FallbackAttempt records a single provider attempt during fallback.
type FallbackAttempt struct {
	ProviderName string
	ErrorType    provider.ErrorType
	Error        error
	LatencyMs    int64
	Success      bool
	Persistent   bool // true if classified as a persistent (quota/billing) failure
}

// FallbackResult holds the outcome of a fallback execution.
type FallbackResult struct {
	Response      any                    // non-stream: successful response
	StreamCh      <-chan domain.SSEChunk // stream: SSE channel
	Route         *router.RouteResult    // the successful route
	Attempts      []FallbackAttempt
	FallbackCount int     // 0 = first attempt succeeded
	FinalError    error   // non-nil if all failed
}

// FallbackEngine executes provider calls with fallback logic.
type FallbackEngine struct {
	health     *provider.HealthTracker
	config     router.FallbackConfig
	classifier *ErrorClassifier // nil → legacy transient-only classification
}

// NewFallbackEngine creates a new fallback engine.
func NewFallbackEngine(health *provider.HealthTracker, config router.FallbackConfig) *FallbackEngine {
	return &FallbackEngine{health: health, config: config}
}

// SetClassifier injects the error classifier used to distinguish persistent (quota/
// billing) failures from transient ones. Nil leaves the legacy transient-only path
// intact (B6: keeps the constructor signature stable for existing callers).
func (e *FallbackEngine) SetClassifier(c *ErrorClassifier) { e.classifier = c }

// classify maps an upstream error to its classification. Only ProviderRows carry an
// AdapterType, so the rule table is consulted solely then; without a classifier the
// error degrades to its legacy ErrorType (always transient).
func (e *FallbackEngine) classify(route *router.RouteResult, err error) ClassifiedError {
	if e.classifier != nil && route.ProviderRow != nil {
		return e.classifier.Classify(route.ProviderRow.AdapterType, err)
	}
	return ClassifiedError{ErrorType: provider.ClassifyError(err)}
}

// retryAfterFrom extracts a Retry-After hint from a *ProviderError, if any.
func retryAfterFrom(err error) time.Duration {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.RetryAfter
	}
	return 0
}

const defaultPerProviderTimeout = 30 * time.Second

func perProviderCtx(parent context.Context, attempt, totalAttempts int) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok {
		remaining := time.Until(deadline)
		remainingAttempts := totalAttempts - attempt
		if remainingAttempts < 1 {
			remainingAttempts = 1
		}
		perProvider := remaining / time.Duration(remainingAttempts)
		if perProvider < 5*time.Second {
			perProvider = 5 * time.Second
		}
		if perProvider > defaultPerProviderTimeout {
			perProvider = defaultPerProviderTimeout
		}
		return context.WithTimeout(parent, perProvider)
	}
	return context.WithTimeout(parent, defaultPerProviderTimeout)
}

// ExecuteNonStream runs a non-streaming call across routes with fallback.
func (e *FallbackEngine) ExecuteNonStream(
	ctx context.Context,
	routes []*router.RouteResult,
	callFn func(ctx context.Context, route *router.RouteResult) (any, error),
) *FallbackResult {
	result := &FallbackResult{}
	maxAttempts := len(routes)
	if e.config.MaxRetries > 0 && e.config.MaxRetries+1 < maxAttempts {
		maxAttempts = e.config.MaxRetries + 1
	}

	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		route := routes[i]
		name := route.Provider.Name()
		model := route.ProviderModel

		if e.health != nil && !e.health.IsHealthyModel(name, model) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: name,
				ErrorType:    provider.ErrorServer,
				Error:        fmt.Errorf("circuit breaker open, skipping"),
				Success:      false,
			})
			lastErr = fmt.Errorf("circuit breaker open, skipping provider %s", name)
			slog.Warn("skipping provider due to circuit breaker", "provider", name)
			continue
		}

		resp, classified, latency, err := e.attemptNonStream(ctx, route, i, maxAttempts, callFn)

		if err != nil {
			attempt := FallbackAttempt{
				ProviderName: name,
				ErrorType:    classified.ErrorType,
				Error:        err,
				LatencyMs:    latency,
				Success:      false,
				Persistent:   classified.Persistent,
			}
			result.Attempts = append(result.Attempts, attempt)

			// Empty ErrorType ⇒ context cancellation/timeout: probe outcome is unknown,
			// so do not record (ClearProbe already released the lease via defer) and stop.
			if classified.ErrorType == "" {
				lastErr = err
				break
			}

			if classified.Persistent {
				if e.health != nil {
					e.health.RecordPersistentFailure(name, model, classified.Scope, 0)
				}
			} else if e.health != nil {
				e.health.RecordTransientFailure(name, model, retryAfterFrom(err))
			}

			if classified.Persistent || shouldRetry(e.config.RetryOn, err) {
				slog.Warn("retryable error, trying next provider",
					"provider", name,
					"error_type", string(classified.ErrorType),
					"persistent", classified.Persistent,
					"scope", classified.Scope,
					"attempt", i+1)
				lastErr = err

				if e.config.RetryDelayMs > 0 && i+1 < maxAttempts {
					timer := time.NewTimer(time.Duration(e.config.RetryDelayMs) * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						result.FinalError = ctx.Err()
						return result
					case <-timer.C:
					}
				}
				continue
			}

			slog.Warn("non-retryable error, stopping fallback",
				"provider", name,
				"error_type", string(classified.ErrorType),
				"persistent", classified.Persistent,
				"error", err)
			lastErr = err
			break
		}

		// Success
		result.Attempts = append(result.Attempts, FallbackAttempt{
			ProviderName: name,
			LatencyMs:    latency,
			Success:      true,
		})
		result.Response = resp
		result.Route = route
		result.FallbackCount = i
		return result
	}

	result.FinalError = lastErr
	return result
}

// attemptNonStream runs a single non-streaming attempt. The probe lease acquired by the
// IsHealthyModel gate is released via defer, so it cannot leak across iterations even
// when the call is cancelled or panics (B1/C2). Success is recorded here; the caller
// records failures from the returned classification.
func (e *FallbackEngine) attemptNonStream(
	ctx context.Context,
	route *router.RouteResult,
	attempt, maxAttempts int,
	callFn func(context.Context, *router.RouteResult) (any, error),
) (resp any, classified ClassifiedError, latency int64, err error) {
	name := route.Provider.Name()
	model := route.ProviderModel
	if e.health != nil {
		defer e.health.ClearProbe(name, model)
	}

	start := time.Now()
	callCtx, callCancel := perProviderCtx(ctx, attempt, maxAttempts)
	if attempt > 0 {
		callCtx = upstream.WithFallback(callCtx, true)
	}
	resp, err = callFn(callCtx, route)
	callCancel()
	latency = time.Since(start).Milliseconds()

	if err != nil {
		classified = e.classify(route, err)
		return resp, classified, latency, err
	}
	if e.health != nil {
		e.health.RecordSuccessModel(name, model)
	}
	return resp, ClassifiedError{}, latency, nil
}

// ExecuteStream runs a streaming connection attempt across routes with fallback.
// Only the connection phase gets fallback — once SSE starts, no more fallback.
func (e *FallbackEngine) ExecuteStream(
	ctx context.Context,
	routes []*router.RouteResult,
	connectFn func(ctx context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error),
) *FallbackResult {
	result := &FallbackResult{}
	maxAttempts := len(routes)
	if e.config.MaxRetries > 0 && e.config.MaxRetries+1 < maxAttempts {
		maxAttempts = e.config.MaxRetries + 1
	}

	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		route := routes[i]
		name := route.Provider.Name()
		model := route.ProviderModel

		if e.health != nil && !e.health.IsHealthyModel(name, model) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: name,
				ErrorType:    provider.ErrorServer,
				Error:        fmt.Errorf("circuit breaker open, skipping"),
				Success:      false,
			})
			lastErr = fmt.Errorf("circuit breaker open, skipping provider %s", name)
			slog.Warn("skipping provider due to circuit breaker", "provider", name)
			continue
		}

		ch, classified, err := e.attemptStream(ctx, route, i, maxAttempts, connectFn)
		if err != nil {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: name,
				ErrorType:    classified.ErrorType,
				Error:        err,
				Success:      false,
				Persistent:   classified.Persistent,
			})

			if classified.ErrorType == "" {
				lastErr = err
				break
			}

			if classified.Persistent {
				if e.health != nil {
					e.health.RecordPersistentFailure(name, model, classified.Scope, 0)
				}
			} else if e.health != nil {
				e.health.RecordTransientFailure(name, model, retryAfterFrom(err))
			}

			if classified.Persistent || shouldRetry(e.config.RetryOn, err) {
				lastErr = err

				if e.config.RetryDelayMs > 0 && i+1 < maxAttempts {
					timer := time.NewTimer(time.Duration(e.config.RetryDelayMs) * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						result.FinalError = ctx.Err()
						return result
					case <-timer.C:
					}
				}
				continue
			}

			lastErr = err
			break
		}

		// Stream connection succeeded
		result.Attempts = append(result.Attempts, FallbackAttempt{
			ProviderName: name,
			Success:      true,
		})
		result.StreamCh = ch
		result.Route = route
		result.FallbackCount = i
		return result
	}

	result.FinalError = lastErr
	return result
}

// attemptStream runs a single streaming connection attempt. The probe lease acquired by
// the IsHealthyModel gate is released via defer (B1/C2). Success is recorded here; the
// caller records failures from the returned classification.
func (e *FallbackEngine) attemptStream(
	ctx context.Context,
	route *router.RouteResult,
	attempt, maxAttempts int,
	connectFn func(context.Context, *router.RouteResult) (<-chan domain.SSEChunk, error),
) (ch <-chan domain.SSEChunk, classified ClassifiedError, err error) {
	name := route.Provider.Name()
	model := route.ProviderModel
	if e.health != nil {
		defer e.health.ClearProbe(name, model)
	}

	connectCtx, connectCancel := perProviderCtx(ctx, attempt, maxAttempts)
	// Use detached context for SSE reading so canceling connectCtx
	// after the HTTP handshake does not kill the response body mid-stream.
	streamCtx := context.WithoutCancel(connectCtx)
	if attempt > 0 {
		streamCtx = upstream.WithFallback(streamCtx, true)
	}
	ch, err = connectFn(streamCtx, route)
	connectCancel()
	if err != nil {
		return nil, e.classify(route, err), err
	}
	if e.health != nil {
		e.health.RecordSuccessModel(name, model)
	}
	return ch, ClassifiedError{}, nil
}

// shouldRetry checks if the error type is in the retry-on list.
// An empty retry-on list uses the default retryable set.
func shouldRetry(retryOn []string, err error) bool {
	errType := provider.ClassifyError(err)
	// Empty string means context.Canceled or nil — not retryable
	if errType == "" {
		return false
	}
	if len(retryOn) == 0 {
		// Default: delegate to IsRetryableError
		return provider.IsRetryableError(err)
	}
	for _, t := range retryOn {
		if string(errType) == t {
			return true
		}
	}
	return false
}

// ResolveFallbackConfig extracts and normalizes fallback config from routes.
func ResolveFallbackConfig(routes []*router.RouteResult) router.FallbackConfig {
	if len(routes) == 0 {
		return router.FallbackConfig{}
	}
	cfg := routes[0].FallbackConfig
	if len(cfg.RetryOn) == 0 {
		cfg.RetryOn = []string{"rate_limit", "server", "network", "timeout"}
	}
	return cfg
}

// ExpandFallbackRoutes resolves FallbackModels from the first route and appends them.
// Deduplicates by (provider_name, provider_model) to avoid redundant attempts.
func ExpandFallbackRoutes(ctx context.Context, resolver RouteResolver, routes []*router.RouteResult, orgID int64) []*router.RouteResult {
	if len(routes) == 0 || resolver == nil {
		return routes
	}
	seen := make(map[string]struct{}, len(routes))
	for _, r := range routes {
		seen[r.Provider.Name()+"|"+r.ProviderModel] = struct{}{}
	}
	for _, fbModel := range routes[0].FallbackModels {
		fbRoutes, err := resolver.Resolve(ctx, fbModel, orgID)
		if err != nil {
			continue
		}
		for _, r := range fbRoutes {
			key := r.Provider.Name() + "|" + r.ProviderModel
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			routes = append(routes, r)
		}
	}
	return routes
}

// LastAttemptRoute returns the provider name of the last attempted route.
// Falls back to routes[0] if no attempts were recorded.
func LastAttemptRoute(result *FallbackResult, routes []*router.RouteResult) *router.RouteResult {
	if result.Route != nil {
		return result.Route
	}
	if len(result.Attempts) > 0 {
		for i := len(result.Attempts) - 1; i >= 0; i-- {
			if result.Attempts[i].Error != nil {
				// Find the route matching this provider name
				for _, r := range routes {
					if r.Provider.Name() == result.Attempts[i].ProviderName {
						return r
					}
				}
			}
		}
	}
	if len(routes) > 0 {
		return routes[0]
	}
	return nil
}
