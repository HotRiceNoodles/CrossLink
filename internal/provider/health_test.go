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

func TestSnapshot_Empty(t *testing.T) {
	ht := NewHealthTracker()
	if got := ht.Snapshot(); len(got) != 0 {
		t.Fatalf("empty tracker snapshot = %v, want empty", got)
	}
}

func TestSnapshot_States(t *testing.T) {
	ht := NewHealthTrackerWithConfig(1, time.Minute)
	ht.RecordTransientFailure("acct", "", 0) // threshold=1 → open
	ht.RecordPersistentFailure("acct", "m1", "model", time.Hour)
	ht.RecordTransientFailure("expired", "", 0)

	snaps := ht.Snapshot()
	if len(snaps) != 3 {
		t.Fatalf("snapshot length = %d, want 3: %+v", len(snaps), snaps)
	}
	byKey := map[string]ProviderHealthSnapshot{}
	for _, s := range snaps {
		k := s.Provider
		if s.Model != "" {
			k += "|" + s.Model
		}
		byKey[k] = s
	}
	if s := byKey["acct"]; s.State != "open" || s.Until.IsZero() {
		t.Errorf("acct = %+v, want open with Until", s)
	}
	if s := byKey["acct|m1"]; s.State != "open" || s.Model != "m1" {
		t.Errorf("acct|m1 = %+v, want open model m1", s)
	}
	if s := byKey["expired"]; s.State != "open" {
		t.Errorf("expired = %+v, want open", s)
	}
}

func TestSnapshot_ConcurrentReadWrite(t *testing.T) {
	ht := NewHealthTrackerWithConfig(1, time.Minute)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			ht.RecordTransientFailure("p", "", 0)
			ht.RecordSuccess("p")
			ht.RecordPersistentFailure("p", "m", "model", time.Second)
		}
	}()
	for i := 0; i < 2000; i++ {
		for _, s := range ht.Snapshot() {
			_ = s.State
		}
	}
	<-done
}
