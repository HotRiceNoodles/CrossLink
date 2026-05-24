package provider

import (
	"testing"
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
