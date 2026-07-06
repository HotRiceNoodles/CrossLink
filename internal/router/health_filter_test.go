package router

import "testing"

// Reuses mockProvider (internal/router/mock_test.go). filterByHealth only
// calls Provider.Name().

func mkHealthCandidate(name string, weight int) RouteCandidate {
	return RouteCandidate{
		Provider: &mockProvider{name: name},
		Weight:   weight,
	}
}

func TestFilterByHealth_NilIsNoOp(t *testing.T) {
	primaries := []RouteCandidate{mkHealthCandidate("A", 1), mkHealthCandidate("B", 1)}
	fallbacks := []RouteCandidate{mkHealthCandidate("C", 0)}
	p, f := filterByHealth(nil, primaries, fallbacks)
	if len(p) != 2 || len(f) != 1 {
		t.Errorf("nil healthFn must be a no-op: got primaries=%d fallbacks=%d", len(p), len(f))
	}
}

func TestFilterByHealth_DemotesZeros(t *testing.T) {
	// A and B healthy (score>0), C unhealthy (score 0) -> C demoted to fallback.
	primaries := []RouteCandidate{mkHealthCandidate("A", 1), mkHealthCandidate("B", 1), mkHealthCandidate("C", 1)}
	healthFn := func(name, _ string) float64 {
		if name == "C" {
			return 0
		}
		return 1.0
	}
	p, f := filterByHealth(healthFn, primaries, nil)
	if len(p) != 2 {
		t.Fatalf("expected 2 healthy primaries, got %d", len(p))
	}
	if len(f) != 1 || f[0].Provider.Name() != "C" {
		t.Fatalf("expected C demoted to fallback, got %v", f)
	}
}

func TestFilterByHealth_AllUnhealthyKeepsOriginals(t *testing.T) {
	// If every primary is health=0, keep them rather than returning an empty
	// primary pool (which would collapse selection to fallback-only / 503).
	primaries := []RouteCandidate{mkHealthCandidate("A", 1), mkHealthCandidate("B", 1)}
	healthFn := func(_, _ string) float64 { return 0 }
	p, f := filterByHealth(healthFn, primaries, nil)
	if len(p) != 2 {
		t.Errorf("all-unhealthy must keep originals: got primaries=%d", len(p))
	}
	if len(f) != 0 {
		t.Errorf("fallbacks must stay empty: got %d", len(f))
	}
}

func TestFilterByHealth_EmptyPrimariesNoOp(t *testing.T) {
	healthFn := func(_, _ string) float64 { return 0 }
	p, f := filterByHealth(healthFn, nil, nil)
	if len(p) != 0 || len(f) != 0 {
		t.Errorf("empty primaries must stay empty")
	}
}
