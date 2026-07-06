package provider

import (
	"testing"
	"time"
)

func TestCircuitState_ClosedByDefault(t *testing.T) {
	h := NewHealthTracker()
	if st := h.CircuitState("zhipu", "glm-5.2"); st != CircuitClosed {
		t.Errorf("expected CircuitClosed, got %v", st)
	}
}

func TestCircuitState_OpenAfterTransientFailures(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 60*time.Second)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-5.2", 0)
	}
	if st := h.CircuitState("zhipu", "glm-5.2"); st != CircuitOpen {
		t.Errorf("expected CircuitOpen after threshold failures, got %v", st)
	}
}

func TestCircuitState_HalfOpenAfterCooldown(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 30*time.Millisecond)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-5.2", 0)
	}
	if st := h.CircuitState("zhipu", "glm-5.2"); st != CircuitOpen {
		t.Fatalf("expected Open before cooldown, got %v", st)
	}
	time.Sleep(40 * time.Millisecond)
	// Cooldown expired but no success yet -> half-open. CircuitState must NOT
	// consume the probe (pure read); IsHealthyModel still works normally.
	if st := h.CircuitState("zhipu", "glm-5.2"); st != CircuitHalfOpen {
		t.Errorf("expected CircuitHalfOpen after cooldown, got %v", st)
	}
}

func TestCircuitState_DoesNotDisturbProbeSingleFlight(t *testing.T) {
	// CircuitState is a pure read: calling it must not set/consume probeInFlight,
	// otherwise it would break IsHealthyModel's single-flight gating.
	h := NewHealthTrackerWithConfig(3, 30*time.Millisecond)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("zhipu", "glm-5.2", 0)
	}
	time.Sleep(40 * time.Millisecond) // -> half-open window

	// Observe state many times.
	for i := 0; i < 5; i++ {
		_ = h.CircuitState("zhipu", "glm-5.2")
	}
	// IsHealthyModel must still allow exactly one probe (return true once).
	first := h.IsHealthyModel("zhipu", "glm-5.2")
	second := h.IsHealthyModel("zhipu", "glm-5.2")
	if !first {
		t.Error("first IsHealthyModel after cooldown should be allowed to probe")
	}
	if second {
		t.Error("second IsHealthyModel should be blocked by probeInFlight single-flight")
	}
}
