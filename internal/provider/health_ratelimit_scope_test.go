package provider

import (
	"testing"
	"time"
)

// Failure-domain contract (soak1 attribution): one provider account ("zhipu")
// serves two upstream models. A rate-limited model must NOT block its sibling;
// network/5xx faults on one model still block the whole account (host-level
// signal). 16,779 collateral 404s (10.84% of normal-flow traffic) were traced
// to the pre-fix account-scoped recording of 429s.

func TestRecordRateLimitFailure_ModelScopedDoesNotBlockSibling(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, time.Hour)
	for i := 0; i < 3; i++ {
		h.RecordRateLimitFailure("zhipu", "glm-a", 0)
	}
	if st := h.CircuitState("zhipu", "glm-a"); st != CircuitOpen {
		t.Fatalf("glm-a circuit must be open after 3 rate limits, got %v", st)
	}
	if st := h.CircuitState("zhipu", "glm-b"); st != CircuitClosed {
		t.Fatalf("sibling glm-b must stay closed (per-model rate-limit domain), got %v", st)
	}
	if st := h.CircuitState("zhipu", ""); st != CircuitClosed {
		t.Fatalf("account key must stay closed (429 is model-scoped), got %v", st)
	}
	if !h.IsHealthyModel("zhipu", "glm-b") {
		t.Fatal("healthy sibling model must remain routable")
	}
}

func TestRecordRateLimitFailure_EmptyModelFallsBackToAccountKey(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, time.Hour)
	for i := 0; i < 3; i++ {
		h.RecordRateLimitFailure("zhipu", "", 0)
	}
	if h.IsHealthyModel("zhipu", "glm-b") {
		t.Fatal("account-wide rate limit (no model attributed) must block all models")
	}
}

// Network/5xx transient failures keep the historical account-wide failure
// domain: a dead host or bad credential is unhealthy for every model on it.
func TestRecordTransientFailure_AccountScopePreserved(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, time.Hour)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-a", 0)
	}
	if h.IsHealthyModel("zhipu", "glm-b") {
		t.Fatal("account circuit open must block sibling models (documented network/5xx failure domain)")
	}
}

// A sibling model's success must not clear another model's rate-limit
// circuit; the rate-limited model's own success clears it (RecordSuccessModel
// deletes the model key unconditionally).
func TestRecordRateLimitFailure_SiblingSuccessDoesNotClear(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, time.Hour)
	for i := 0; i < 3; i++ {
		h.RecordRateLimitFailure("zhipu", "glm-a", 0)
	}
	h.RecordSuccessModel("zhipu", "glm-b")
	if st := h.CircuitState("zhipu", "glm-a"); st != CircuitOpen {
		t.Fatalf("glm-a rate-limit circuit must survive sibling success, got %v", st)
	}
	h.RecordSuccessModel("zhipu", "glm-a")
	if st := h.CircuitState("zhipu", "glm-a"); st != CircuitClosed {
		t.Fatalf("glm-a success must clear its own circuit, got %v", st)
	}
}

// The rate-limit model circuit follows the same threshold and Retry-After
// clamping contract as the account circuit.
func TestRecordRateLimitFailure_ThresholdAndRetryAfter(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, time.Hour)
	h.SetRetryAfterBounds(2*time.Second, 2*time.Second)
	h.RecordRateLimitFailure("zhipu", "glm-a", 0)
	h.RecordRateLimitFailure("zhipu", "glm-a", 0)
	if st := h.CircuitState("zhipu", "glm-a"); st != CircuitClosed {
		t.Fatalf("2 failures below threshold must not open, got %v", st)
	}
	h.RecordRateLimitFailure("zhipu", "glm-a", 90*time.Second) // clamped to 2s max
	if st := h.CircuitState("zhipu", "glm-a"); st != CircuitOpen {
		t.Fatalf("3rd failure must open, got %v", st)
	}
	if d := h.OpenCircuitDescription("zhipu", "glm-a"); d == "" {
		t.Fatal("open circuit must describe itself")
	}
}
