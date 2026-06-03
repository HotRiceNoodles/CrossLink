package service

import (
	"context"
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
	health *provider.HealthTracker
	config router.FallbackConfig
}

// NewFallbackEngine creates a new fallback engine.
func NewFallbackEngine(health *provider.HealthTracker, config router.FallbackConfig) *FallbackEngine {
	return &FallbackEngine{health: health, config: config}
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

		if e.health != nil && !e.health.IsHealthy(route.Provider.Name()) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: route.Provider.Name(),
				ErrorType:    provider.ErrorServer,
				Error:        fmt.Errorf("circuit breaker open, skipping"),
				Success:      false,
			})
			lastErr = fmt.Errorf("circuit breaker open, skipping provider %s", route.Provider.Name())
			slog.Warn("skipping provider due to circuit breaker", "provider", route.Provider.Name())
			continue
		}

		start := time.Now()
		callCtx, callCancel := perProviderCtx(ctx, i, maxAttempts)
		if i > 0 {
				callCtx = upstream.WithFallback(callCtx, true)
			}
		resp, err := callFn(callCtx, route)
		callCancel()
		latency := time.Since(start).Milliseconds()

		if err != nil {
			errType := provider.ClassifyError(err)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: route.Provider.Name(),
				ErrorType:    errType,
				Error:        err,
				LatencyMs:    latency,
				Success:      false,
			})

			if e.health != nil {
				e.health.RecordFailure(route.Provider.Name())
			}

			if !shouldRetry(e.config.RetryOn, err) {
				slog.Warn("non-retryable error, stopping fallback",
					"provider", route.Provider.Name(),
					"error_type", string(errType),
					"error", err)
				lastErr = err
				break
			}

			slog.Warn("retryable error, trying next provider",
				"provider", route.Provider.Name(),
				"error_type", string(errType),
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

		// Success
		if e.health != nil {
			e.health.RecordSuccess(route.Provider.Name())
		}
		result.Attempts = append(result.Attempts, FallbackAttempt{
			ProviderName: route.Provider.Name(),
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

		if e.health != nil && !e.health.IsHealthy(route.Provider.Name()) {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: route.Provider.Name(),
				ErrorType:    provider.ErrorServer,
				Error:        fmt.Errorf("circuit breaker open, skipping"),
				Success:      false,
			})
			lastErr = fmt.Errorf("circuit breaker open, skipping provider %s", route.Provider.Name())
			slog.Warn("skipping provider due to circuit breaker", "provider", route.Provider.Name())
			continue
		}

		connectCtx, connectCancel := perProviderCtx(ctx, i, maxAttempts)
		// Use detached context for SSE reading so canceling connectCtx
		// after the HTTP handshake does not kill the response body mid-stream.
		streamCtx := context.WithoutCancel(connectCtx)
		if i > 0 {
				streamCtx = upstream.WithFallback(streamCtx, true)
			}
		ch, err := connectFn(streamCtx, route)
		connectCancel()
		if err != nil {
			errType := provider.ClassifyError(err)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName: route.Provider.Name(),
				ErrorType:    errType,
				Error:        err,
				Success:      false,
			})

			if e.health != nil {
				e.health.RecordFailure(route.Provider.Name())
			}

			if !shouldRetry(e.config.RetryOn, err) {
				lastErr = err
				break
			}

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

		// Stream connection succeeded
		if e.health != nil {
			e.health.RecordSuccess(route.Provider.Name())
		}
		result.Attempts = append(result.Attempts, FallbackAttempt{
			ProviderName: route.Provider.Name(),
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
