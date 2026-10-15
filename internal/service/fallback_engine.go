package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"go.opentelemetry.io/otel"
	otelattr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// RouteResolver resolves model names to route results.
type RouteResolver interface {
	Resolve(ctx context.Context, modelName string, orgID int64) ([]*router.RouteResult, error)
}

// FallbackAttempt records a single provider attempt during fallback.
type FallbackAttempt struct {
	ProviderName  string
	ProviderModel string
	ErrorType     provider.ErrorType
	Error         error
	LatencyMs     int64
	Success       bool
	Persistent    bool // true if classified as a persistent (quota/billing) failure
	// UpstreamStatus is the provider's HTTP status when the error carried one
	// (0 = network/timeout/cancel, no upstream response). Persisted in the
	// usage log's attempts timeline (error observability L2).
	UpstreamStatus int
}

// FallbackResult holds the outcome of a fallback execution.
type FallbackResult struct {
	Response      any                    // non-stream: successful response
	StreamCh      <-chan domain.SSEChunk // stream: SSE channel
	CancelStream  func()                 // releases the whole-stream deadline timer; nil unless StreamMaxDuration > 0
	Route         *router.RouteResult    // the successful route
	Attempts      []FallbackAttempt
	FallbackCount int   // 0 = first attempt succeeded
	FinalError    error // non-nil if all failed
}

// streamMaxDuration caps the total duration of one SSE stream when > 0
// (0 = uncapped, the default). Package-level because engines are constructed
// per request; wired from server config once at startup via
// SetStreamMaxDuration.
var streamMaxDuration atomic.Int64 // nanoseconds; 0 = disabled

// SetStreamMaxDuration configures the whole-stream cap (<= 0 disables it).
func SetStreamMaxDuration(d time.Duration) { streamMaxDuration.Store(int64(d)) }

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

// defaultPerProviderTimeout bounds one non-streaming upstream attempt. Long
// reasoning-model generations regularly exceed 30s, so the effective value is
// wired from gateway.provider_timeout (default 180s) via SetPerProviderTimeout
// at startup; this constant is only the pre-wiring fallback.
const defaultPerProviderTimeout = 30 * time.Second

// perProviderTimeout is the effective per-attempt cap (nanoseconds; 0 = use
// the constant above). Package-level because engines are constructed per
// request.
var perProviderTimeout atomic.Int64

// SetPerProviderTimeout configures the non-streaming per-attempt cap.
// Non-positive values fall back to the built-in default.
func SetPerProviderTimeout(d time.Duration) {
	if d <= 0 {
		perProviderTimeout.Store(0)
		return
	}
	perProviderTimeout.Store(int64(d))
}

func effectivePerProviderTimeout() time.Duration {
	if d := time.Duration(perProviderTimeout.Load()); d > 0 {
		return d
	}
	return defaultPerProviderTimeout
}

// circuitSkipError builds the error for a circuit-breaker skip, carrying the
// recorded upstream cause (error type + message from RecordCause) when
// available so logs and the final error explain WHY the provider was skipped.
func circuitSkipError(h *provider.HealthTracker, name, model string) error {
	if h != nil {
		if d := h.OpenCircuitDescription(name, model); d != "" {
			return fmt.Errorf("circuit breaker open, skipping provider: %s", d)
		}
	}
	return fmt.Errorf("circuit breaker open, skipping provider %s", name)
}

// upstreamStatusOf extracts the provider's HTTP status from an attempt error
// (0 = network/timeout/cancel with no upstream response).
func upstreamStatusOf(err error) int {
	var pe *provider.ProviderError
	if errors.As(err, &pe) {
		return pe.StatusCode
	}
	return 0
}

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
		cap := effectivePerProviderTimeout()
		if perProvider > cap {
			perProvider = cap
		}
		return context.WithTimeout(parent, perProvider)
	}
	return context.WithTimeout(parent, effectivePerProviderTimeout())
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
			skipErr := circuitSkipError(e.health, name, model)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName:  name,
				ProviderModel: model,
				ErrorType:     provider.ErrorServer,
				Error:         skipErr,
				Success:       false,
			})
			lastErr = skipErr
			slog.Warn("skipping provider due to circuit breaker", "provider", name,
				"reason", skipErr.Error())
			continue
		}

		resp, classified, latency, err := e.attemptNonStream(ctx, route, i, maxAttempts, callFn)

		if err != nil {
			attempt := FallbackAttempt{
				ProviderName:   name,
				ProviderModel:  model,
				ErrorType:      classified.ErrorType,
				Error:          err,
				LatencyMs:      latency,
				Success:        false,
				Persistent:     classified.Persistent,
				UpstreamStatus: upstreamStatusOf(err),
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
					e.health.RecordCause(name, model, string(classified.ErrorType), err.Error())
				}
			} else if e.health != nil {
				if classified.ErrorType == provider.ErrorRateLimit {
					// Rate limits are per-model: open the MODEL circuit, not the
					// shared account circuit (isolation — see RecordRateLimitFailure).
					e.health.RecordRateLimitFailure(name, model, retryAfterFrom(err))
				} else {
					e.health.RecordTransientFailure(name, model, retryAfterFrom(err))
				}
				e.health.RecordCause(name, model, string(classified.ErrorType), err.Error())
			}

			if classified.Persistent || shouldRetry(e.config.RetryOn, err) {
				slog.Warn("retryable error, trying next provider",
					"provider", name,
					"error_type", string(classified.ErrorType),
					"persistent", classified.Persistent,
					"scope", classified.Scope,
					"rule_id", classified.RuleID,
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
			ProviderName:  name,
			ProviderModel: model,
			LatencyMs:     latency,
			Success:       true,
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
	// Create a provider child span so Jaeger/Tempo shows gateway vs upstream latency.
	tracer := otel.Tracer("github.com/crosslink/fallback")
	callCtx, span := tracer.Start(callCtx, "provider.chat",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			otelattr.String("llm.provider", name),
			otelattr.String("llm.model", model),
		),
	)
	defer span.End()
	resp, err = callFn(callCtx, route)
	callCancel()
	latency = time.Since(start).Milliseconds()

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		classified = e.classify(route, err)
		return resp, classified, latency, err
	}
	span.SetAttributes(otelattr.Int64("llm.latency_ms", latency))
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
			skipErr := circuitSkipError(e.health, name, model)
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName:  name,
				ProviderModel: model,
				ErrorType:     provider.ErrorServer,
				Error:         skipErr,
				Success:       false,
			})
			lastErr = skipErr
			slog.Warn("skipping provider due to circuit breaker", "provider", name,
				"reason", skipErr.Error())
			continue
		}

		ch, cancelStream, classified, err := e.attemptStream(ctx, route, i, maxAttempts, connectFn)
		if err != nil {
			result.Attempts = append(result.Attempts, FallbackAttempt{
				ProviderName:   name,
				ProviderModel:  model,
				ErrorType:      classified.ErrorType,
				Error:          err,
				Success:        false,
				Persistent:     classified.Persistent,
				UpstreamStatus: upstreamStatusOf(err),
			})

			if classified.ErrorType == "" {
				lastErr = err
				break
			}

			if classified.Persistent {
				if e.health != nil {
					e.health.RecordPersistentFailure(name, model, classified.Scope, 0)
					e.health.RecordCause(name, model, string(classified.ErrorType), err.Error())
				}
			} else if e.health != nil {
				if classified.ErrorType == provider.ErrorRateLimit {
					// Rate limits are per-model: open the MODEL circuit, not the
					// shared account circuit (isolation — see RecordRateLimitFailure).
					e.health.RecordRateLimitFailure(name, model, retryAfterFrom(err))
				} else {
					e.health.RecordTransientFailure(name, model, retryAfterFrom(err))
				}
				e.health.RecordCause(name, model, string(classified.ErrorType), err.Error())
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
			ProviderName:  name,
			ProviderModel: model,
			Success:       true,
		})
		result.StreamCh = ch
		result.CancelStream = cancelStream
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
) (ch <-chan domain.SSEChunk, cancelStream context.CancelFunc, classified ClassifiedError, err error) {
	name := route.Provider.Name()
	model := route.ProviderModel
	if e.health != nil {
		defer e.health.ClearProbe(name, model)
	}

	// The stream ctx derives from the request ctx so client disconnects
	// propagate to the upstream read. It previously derived from the
	// per-provider connect ctx via context.WithoutCancel, which detached the
	// disconnect signal AND every deadline — leaking the reader goroutine and
	// the upstream connection on disconnects. The connect timeout no longer
	// wraps the stream: the handshake is bounded by the transport's
	// ResponseHeaderTimeout, and canceling a per-attempt ctx after the
	// handshake would kill the response body mid-stream.
	streamCtx := ctx
	if d := time.Duration(streamMaxDuration.Load()); d > 0 {
		streamCtx, cancelStream = context.WithTimeout(ctx, d)
	}
	if attempt > 0 {
		streamCtx = upstream.WithFallback(streamCtx, true)
	}
	// Provider child span for stream path.
	tracer := otel.Tracer("github.com/crosslink/fallback")
	streamCtx, span := tracer.Start(streamCtx, "provider.stream",
		oteltrace.WithSpanKind(oteltrace.SpanKindClient),
		oteltrace.WithAttributes(
			otelattr.String("llm.provider", name),
			otelattr.String("llm.model", model),
		),
	)
	defer span.End()
	ch, err = connectFn(streamCtx, route)
	if err != nil {
		if cancelStream != nil {
			cancelStream()
		}
		span.SetStatus(codes.Error, err.Error())
		return nil, nil, e.classify(route, err), err
	}
	if e.health != nil {
		e.health.RecordSuccessModel(name, model)
	}
	return ch, cancelStream, ClassifiedError{}, nil
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
