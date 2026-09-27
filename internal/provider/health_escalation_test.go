package provider

import (
	"testing"
	"time"
)

func TestEscalatedCooldown_Sequence(t *testing.T) {
	h := NewHealthTrackerWithConfig(1, 10*time.Second)
	h.SetMaxEscalation(40 * time.Second)
	cases := []struct {
		openCount int
		want      time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 40 * time.Second}, // capped
		{100, 40 * time.Second},
	}
	for _, c := range cases {
		if got := h.escalatedCooldown(c.openCount); got != c.want {
			t.Errorf("escalatedCooldown(%d) = %v, want %v", c.openCount, got, c.want)
		}
	}
}

// Repeated open→expire→fail cycles escalate the cooldown; recovery (a success)
// resets it back to the base.
func TestRecordTransientFailure_EscalationAcrossReopens(t *testing.T) {
	h := NewHealthTrackerWithConfig(1, 10*time.Millisecond)
	h.SetMaxEscalation(80 * time.Millisecond)

	// First open: base cooldown.
	h.RecordTransientFailure("X", "", 0)
	snap := h.Snapshot()[0]
	if snap.OpenCount != 1 {
		t.Fatalf("OpenCount after first open = %d, want 1", snap.OpenCount)
	}
	firstUntil := snap.Until

	time.Sleep(15 * time.Millisecond) // expire → half-open

	// Half-open probe failure: re-arms without opening.
	h.RecordTransientFailure("X", "", 0)
	// Next failure opens again with escalation (2× base).
	h.RecordTransientFailure("X", "", 0)
	snap = h.Snapshot()[0]
	if snap.OpenCount != 2 {
		t.Fatalf("OpenCount after second open = %d, want 2", snap.OpenCount)
	}
	if !snap.Until.After(firstUntil.Add(2 * time.Millisecond)) {
		t.Fatalf("second open should cool down longer: until=%v, first=%v", snap.Until, firstUntil)
	}

	// Recovery resets the escalation.
	time.Sleep(25 * time.Millisecond) // expire the 20ms escalation
	h.RecordSuccessModel("X", "")
	h.RecordTransientFailure("X", "", 0)
	snap = h.Snapshot()[0]
	if snap.OpenCount != 1 {
		t.Fatalf("OpenCount after recovery = %d, want 1 (reset)", snap.OpenCount)
	}
}

// Retry-After hints bypass escalation: the upstream knows best.
func TestRecordTransientFailure_RetryAfterNotEscalated(t *testing.T) {
	h := NewHealthTrackerWithConfig(1, 10*time.Millisecond)
	h.SetMaxEscalation(40 * time.Second)
	h.SetRetryAfterBounds(50*time.Millisecond, 5*time.Minute)

	h.RecordTransientFailure("X", "", 0) // open 1: base 10ms
	time.Sleep(15 * time.Millisecond)   // expire → half-open
	h.RecordTransientFailure("X", "", 0) // half-open failure: re-armed
	h.RecordTransientFailure("X", "", 0) // open 2: escalated 20ms
	snap := h.Snapshot()[0]
	if got := time.Until(snap.Until); got < 10*time.Millisecond || got > 25*time.Millisecond {
		t.Fatalf("second open should be ~20ms, got %v", got)
	}

	time.Sleep(25 * time.Millisecond)                    // expire the 20ms escalation
	h.RecordTransientFailure("X", "", 0)                 // half-open failure
	h.RecordTransientFailure("X", "", 200*time.Millisecond) // open 3, hinted: clamp → 200ms (not 40s escalated)
	snap = h.Snapshot()[0]
	if got := time.Until(snap.Until); got < 150*time.Millisecond || got > 230*time.Millisecond {
		t.Fatalf("hinted open should be ~200ms (not the 40s escalation cap), got %v", got)
	}
}
