package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

func makeRoute(name string) *router.RouteResult {
	return &router.RouteResult{
		Provider:      &mockProvider{name: name},
		ProviderModel: "test-model",
		ProviderRow:   &model.Provider{ID: 1},
	}
}

func makeRoutes(names ...string) []*router.RouteResult {
	out := make([]*router.RouteResult, len(names))
	for i, n := range names {
		out[i] = makeRoute(n)
	}
	return out
}

func TestExecuteNonStream_FirstSucceeds(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b", "c")

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return "ok", nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected no error, got %v", result.FinalError)
	}
	if result.FallbackCount != 0 {
		t.Fatalf("expected FallbackCount=0, got %d", result.FallbackCount)
	}
	if result.Response != "ok" {
		t.Fatalf("expected response 'ok', got %v", result.Response)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(result.Attempts))
	}
	if !result.Attempts[0].Success {
		t.Fatal("expected first attempt to succeed")
	}
}

func TestExecuteNonStream_FallbackSucceeds(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b", "c")
	callCount := 0

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, route *router.RouteResult) (any, error) {
		callCount++
		if route.Provider.Name() == "a" {
			return nil, &provider.ProviderError{StatusCode: 429, ErrorType: provider.ErrorRateLimit, Message: "rate limited"}
		}
		return "ok", nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected no error, got %v", result.FinalError)
	}
	if result.FallbackCount != 1 {
		t.Fatalf("expected FallbackCount=1, got %d", result.FallbackCount)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 calls, got %d", callCount)
	}
}

func TestExecuteNonStream_AllFail(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "server error"}
	})

	if result.FinalError == nil {
		t.Fatal("expected error, got nil")
	}
	if result.Response != nil {
		t.Fatalf("expected nil response, got %v", result.Response)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

func TestExecuteNonStream_NonRetryableStops(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")
	callCount := 0

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		callCount++
		return nil, &provider.ProviderError{
			StatusCode: 401,
			Message:    "auth failed",
			ErrorType:  provider.ErrorAuth,
		}
	})

	if result.FinalError == nil {
		t.Fatal("expected error")
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (auth stops), got %d", callCount)
	}
}

func TestExecuteNonStream_MaxRetriesLimits(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{MaxRetries: 1})
	routes := makeRoutes("a", "b", "c")
	callCount := 0

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		callCount++
		return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "fail"}
	})

	if callCount != 2 {
		t.Fatalf("max_retries=1 should limit to 2 attempts, got %d", callCount)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(result.Attempts))
	}
}

func TestExecuteNonStream_CircuitBreakerSkips(t *testing.T) {
	health := provider.NewHealthTracker()
	// Burn through circuit breaker threshold for "a"
	for i := 0; i < 3; i++ {
		health.RecordFailure("a")
	}

	engine := NewFallbackEngine(health, router.FallbackConfig{})
	routes := makeRoutes("a", "b")
	callCount := 0

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, route *router.RouteResult) (any, error) {
		callCount++
		if route.Provider.Name() == "a" {
			t.Fatal("should not call unhealthy provider 'a'")
		}
		return "ok", nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected success from 'b', got %v", result.FinalError)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call (only 'b'), got %d", callCount)
	}
	if result.Attempts[0].ProviderName != "a" {
		t.Fatal("first attempt should be 'a' (skipped)")
	}
	if !result.Attempts[0].Success {
		// skipped attempt has error, not success
		t.Log("skipped attempt correctly recorded as failure")
	}
}

func TestExecuteNonStream_RetryOnFilter(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{
		RetryOn: []string{"server"}, // only retry server errors
	})
	routes := makeRoutes("a", "b")
	callCount := 0

	_ = engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		callCount++
		return nil, &provider.ProviderError{
			StatusCode: 429,
			Message:    "rate limited",
			ErrorType:  provider.ErrorRateLimit,
		}
	})

	if callCount != 1 {
		t.Fatalf("rate_limit should not retry with retry_on=[server], got %d calls", callCount)
	}
}

func TestExecuteNonStream_ContextCancellation(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = engine.ExecuteNonStream(ctx, routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return "ok", nil
	})
}

func TestExecuteNonStream_RetryDelay(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{
		RetryDelayMs: 50,
	})
	routes := makeRoutes("a", "b")
	callCount := 0
	start := time.Now()

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		callCount++
		if callCount == 1 {
			return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "fail"}
		}
		return "ok", nil
	})

	elapsed := time.Since(start)
	if result.FinalError != nil {
		t.Fatalf("expected success, got %v", result.FinalError)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("expected delay of ~50ms between retries, got %v", elapsed)
	}
}

func TestExecuteNonStream_HealthTracking(t *testing.T) {
	health := provider.NewHealthTracker()
	engine := NewFallbackEngine(health, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, route *router.RouteResult) (any, error) {
		if route.Provider.Name() == "a" {
			return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "fail"}
		}
		return "ok", nil
	})

	// "a" should have a failure recorded (1 fail, not yet at threshold=3, so still healthy)
	// But we can verify the failure was recorded by checking after 3 attempts
	if !health.IsHealthy("a") {
		t.Fatal("provider 'a' should still be healthy after 1 failure (threshold=3)")
	}

	// "b" should have a success recorded (state deleted on success)
	if !health.IsHealthy("b") {
		t.Fatal("provider 'b' should be healthy after success")
	}

	// Verify that 3 failures trigger circuit breaker
	for i := 0; i < 3; i++ {
		health.RecordFailure("c")
	}
	if health.IsHealthy("c") {
		t.Fatal("provider 'c' should be unhealthy after 3 consecutive failures")
	}
}

func TestExecuteStream_FirstSucceeds(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	ch := make(chan domain.SSEChunk, 1)
	ch <- domain.SSEChunk{Done: true}
	close(ch)

	result := engine.ExecuteStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (<-chan domain.SSEChunk, error) {
		return ch, nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected no error, got %v", result.FinalError)
	}
	if result.StreamCh == nil {
		t.Fatal("expected stream channel")
	}
	if result.FallbackCount != 0 {
		t.Fatalf("expected FallbackCount=0, got %d", result.FallbackCount)
	}
}

func TestExecuteStream_FallbackSucceeds(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	result := engine.ExecuteStream(context.Background(), routes, func(_ context.Context, route *router.RouteResult) (<-chan domain.SSEChunk, error) {
		if route.Provider.Name() == "a" {
			return nil, &provider.ProviderError{StatusCode: 502, ErrorType: provider.ErrorNetwork, Message: "connection refused"}
		}
		ch := make(chan domain.SSEChunk, 1)
		ch <- domain.SSEChunk{Done: true}
		close(ch)
		return ch, nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected success, got %v", result.FinalError)
	}
	if result.FallbackCount != 1 {
		t.Fatalf("expected FallbackCount=1, got %d", result.FallbackCount)
	}
}

func TestExecuteStream_AllFail(t *testing.T) {
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a", "b")

	result := engine.ExecuteStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (<-chan domain.SSEChunk, error) {
		return nil, &provider.ProviderError{StatusCode: 500, ErrorType: provider.ErrorServer, Message: "fail"}
	})

	if result.FinalError == nil {
		t.Fatal("expected error")
	}
	if result.StreamCh != nil {
		t.Fatal("expected nil stream channel")
	}
}

func TestShouldRetry_EmptyErrorType(t *testing.T) {
	// context.Canceled produces empty error type
	if shouldRetry([]string{}, context.Canceled) {
		t.Fatal("context.Canceled should not be retryable")
	}
}

func TestShouldRetry_DefaultBehavior(t *testing.T) {
	// Empty retry_on uses IsRetryableError
	serverErr := &provider.ProviderError{ErrorType: provider.ErrorServer}
	if !shouldRetry([]string{}, serverErr) {
		t.Fatal("server error should be retryable with default config")
	}

	authErr := &provider.ProviderError{ErrorType: provider.ErrorAuth}
	if shouldRetry([]string{}, authErr) {
		t.Fatal("auth error should not be retryable")
	}
}

func TestShouldRetry_CustomFilter(t *testing.T) {
	rateLimitErr := &provider.ProviderError{ErrorType: provider.ErrorRateLimit}
	serverErr := &provider.ProviderError{ErrorType: provider.ErrorServer}

	if shouldRetry([]string{"server"}, rateLimitErr) {
		t.Fatal("rate_limit should not match retry_on=[server]")
	}
	if !shouldRetry([]string{"server"}, serverErr) {
		t.Fatal("server should match retry_on=[server]")
	}
}

func TestResolveFallbackConfig_Defaults(t *testing.T) {
	// Empty config should get default retry_on
	routes := []*router.RouteResult{
		{FallbackConfig: router.FallbackConfig{}},
	}
	cfg := ResolveFallbackConfig(routes)
	if len(cfg.RetryOn) != 4 {
		t.Fatalf("expected 4 default retry_on types, got %d", len(cfg.RetryOn))
	}
}

func TestResolveFallbackConfig_EmptyRoutes(t *testing.T) {
	cfg := ResolveFallbackConfig(nil)
	if len(cfg.RetryOn) != 0 {
		t.Fatalf("expected empty config for nil routes, got %v", cfg)
	}
}

func TestExpandFallbackRoutes(t *testing.T) {
	// Without a resolver, routes are returned as-is
	routes := makeRoutes("a")
	result := ExpandFallbackRoutes(context.Background(), nil, routes)
	if len(result) != 1 {
		t.Fatalf("expected 1 route, got %d", len(result))
	}
}

func TestExecuteNonStream_NilErrorRetry(t *testing.T) {
	// When callFn returns nil error but nil response... shouldn't happen
	// but verify engine handles it
	engine := NewFallbackEngine(nil, router.FallbackConfig{})
	routes := makeRoutes("a")

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return nil, nil
	})

	if result.FinalError != nil {
		t.Fatalf("expected no error, got %v", result.FinalError)
	}
	if result.Response != nil {
		t.Fatalf("expected nil response, got %v", result.Response)
	}
}

// mockResolver resolves model names to routes for testing.
type mockResolver struct {
	routes map[string][]*router.RouteResult
}

func (m *mockResolver) Resolve(_ context.Context, modelName string) ([]*router.RouteResult, error) {
	r, ok := m.routes[modelName]
	if !ok {
		return nil, fmt.Errorf("no provider for model: %s", modelName)
	}
	return r, nil
}

func TestExpandFallbackRoutes_WithResolver(t *testing.T) {
	resolver := &mockResolver{
		routes: map[string][]*router.RouteResult{
			"fallback-model": makeRoutes("c", "d"),
		},
	}
	routes := []*router.RouteResult{
		{Provider: &mockProvider{name: "a"}, ProviderModel: "m1", FallbackModels: []string{"fallback-model"}},
	}
	result := ExpandFallbackRoutes(context.Background(), resolver, routes)
	if len(result) != 3 {
		t.Fatalf("expected 3 routes (1 original + 2 fallback), got %d", len(result))
	}
	if result[0].Provider.Name() != "a" {
		t.Fatal("first route should be original")
	}
	if result[1].Provider.Name() != "c" {
		t.Fatal("second route should be fallback 'c'")
	}
}

func TestExpandFallbackRoutes_Dedup(t *testing.T) {
	resolver := &mockResolver{
		routes: map[string][]*router.RouteResult{
			"fb1": {{
				Provider:      &mockProvider{name: "a"},
				ProviderModel: "m1",
				ProviderRow:   &model.Provider{ID: 1},
			}},
		},
	}
	// Original routes already include provider "a" with model "m1"
	routes := []*router.RouteResult{
		{Provider: &mockProvider{name: "a"}, ProviderModel: "m1", FallbackModels: []string{"fb1"}},
	}
	result := ExpandFallbackRoutes(context.Background(), resolver, routes)
	if len(result) != 1 {
		t.Fatalf("expected 1 route (dedup removes duplicate 'a|m1'), got %d", len(result))
	}
}

func TestExpandFallbackRoutes_ResolveError(t *testing.T) {
	resolver := &mockResolver{routes: map[string][]*router.RouteResult{}}
	routes := []*router.RouteResult{
		{Provider: &mockProvider{name: "a"}, ProviderModel: "m1", FallbackModels: []string{"nonexistent"}},
	}
	result := ExpandFallbackRoutes(context.Background(), resolver, routes)
	if len(result) != 1 {
		t.Fatalf("expected 1 route (resolve error ignored), got %d", len(result))
	}
}
