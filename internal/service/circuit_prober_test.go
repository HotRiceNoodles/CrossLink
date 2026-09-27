package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// The fallback engine records the upstream cause on the circuit it opens.
func TestFallbackEngine_RecordsCause(t *testing.T) {
	health := provider.NewHealthTrackerWithConfig(1, time.Minute)
	engine := NewFallbackEngine(health, router.FallbackConfig{})
	routes := makeRoutes("minimax")

	result := engine.ExecuteNonStream(context.Background(), routes, func(_ context.Context, _ *router.RouteResult) (any, error) {
		return nil, &provider.ProviderError{StatusCode: 404, ErrorType: provider.ErrorNotFound, Message: "provider not found: Model not exists"}
	})
	if result.FinalError == nil {
		t.Fatal("expected failure")
	}

	desc := health.OpenCircuitDescription("minimax", "")
	if desc == "" {
		t.Fatal("expected an open circuit with a description after 3 failures")
	}
	if !strings.Contains(desc, "not_found") || !strings.Contains(desc, "Model not exists") {
		t.Fatalf("description %q missing the recorded cause", desc)
	}
}
