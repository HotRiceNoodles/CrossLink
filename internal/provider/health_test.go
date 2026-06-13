package provider

import (
	"testing"
	"time"
)

func TestHealthTracker_HealthyByDefault(t *testing.T) {
	h := NewHealthTracker()
	if !h.IsHealthy("test") {
		t.Error("provider should be healthy by default")
	}
}

func TestHealthTracker_CircuitBreaker(t *testing.T) {
	h := NewHealthTracker()

	for i := 0; i < 3; i++ {
		h.RecordFailure("test")
	}
	if h.IsHealthy("test") {
		t.Error("provider should be unhealthy after 3 failures")
	}

	h.RecordSuccess("test")
	if !h.IsHealthy("test") {
		t.Error("provider should be healthy after success")
	}
}

func TestHealthTracker_LessThanThreshold(t *testing.T) {
	h := NewHealthTracker()
	h.RecordFailure("test")
	h.RecordFailure("test")
	if !h.IsHealthy("test") {
		t.Error("provider should still be healthy after 2 failures (threshold is 3)")
	}
}

// C1: a success on one model must NOT clear a non-expired persistent account circuit
// set by another call (closes the IsHealthy-then-fail race).
func TestRecordSuccessModel_DoesNotClearPersistentAccount(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 20*time.Second)
	h.RecordPersistentFailure("X", "", "account", 30*time.Minute)

	h.RecordSuccessModel("X", "modelB") // unrelated model succeeds

	if h.IsHealthyModel("X", "modelA") {
		t.Fatal("persistent account circuit must survive an unrelated model's success")
	}
}

// C2: when a circuit expires (half-open), exactly one caller is allowed to probe.
func TestIsHealthyModel_SingleFlightHalfOpen(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 1*time.Millisecond)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("X", "", 0)
	}
	time.Sleep(3 * time.Millisecond) // expire → half-open

	allowed := 0
	for i := 0; i < 10; i++ {
		if h.IsHealthyModel("X", "") {
			allowed++
		}
	}
	if allowed != 1 {
		t.Fatalf("expected exactly 1 probe allowed, got %d", allowed)
	}
}

func TestClearProbe_ReleasesLease(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 1*time.Millisecond)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("X", "", 0)
	}
	time.Sleep(3 * time.Millisecond)

	if !h.IsHealthyModel("X", "") {
		t.Fatal("first probe should be allowed")
	}
	if h.IsHealthyModel("X", "") {
		t.Fatal("second call must be blocked while probe in flight")
	}
	h.ClearProbe("X", "")
	if !h.IsHealthyModel("X", "") {
		t.Fatal("after ClearProbe a new probe should be allowed")
	}
}

// model-scope persistent failure only trips that model's key, not the account or siblings.
func TestRecordPersistentFailure_ModelScope_OnlyModelKey(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 20*time.Second)
	h.RecordPersistentFailure("X", "A", "model", time.Minute)

	if !h.IsHealthyModel("X", "B") {
		t.Fatal("sibling model must remain healthy")
	}
	if h.IsHealthyModel("X", "A") {
		t.Fatal("deprecated model must be unhealthy")
	}
}

// Retry-After is clamped to [min, max] when the transient circuit opens.
func TestRecordTransientFailure_RetryAfterClamp(t *testing.T) {
	h := NewHealthTrackerWithConfig(3, 20*time.Second)
	h.SetRetryAfterBounds(5*time.Second, 300*time.Second)
	for i := 0; i < 3; i++ {
		h.RecordTransientFailure("X", "", 9999*time.Second) // clamps to max (300s)
	}
	s := h.states["X"]
	if s == nil || s.openUntil.IsZero() {
		t.Fatal("circuit should be open after threshold")
	}
	got := time.Until(s.openUntil)
	if got > 301*time.Second {
		t.Fatalf("Retry-After not clamped to max: %v", got)
	}
	if got < 290*time.Second {
		t.Fatalf("unexpectedly short cooldown: %v", got)
	}
}
