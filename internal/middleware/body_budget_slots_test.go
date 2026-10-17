package middleware

// Slot sizing sanity for the unknown-length admission gate (kept in its own
// file: it calls unknownSlots(), which exists only in the post-livelock-fix
// code — pre-fix checkouts can still compile and run the livelock barrier
// tests in body_budget_livelock_test.go).
//
// K = limit / (2×C_worst + block), floored at 1.

import "testing"

func TestUnknownAdmission_SlotSizing(t *testing.T) {
	for _, tc := range []struct {
		limit, maxSize, wantK int64
	}{
		{3 << 20, 10 << 20, 1},    // ceiling 1.25MiB -> 3MiB/3MiB = 1
		{1 << 30, 10 << 20, 49},   // prod shape: 1GiB / ~20.5MiB = 49
		{256 << 20, 10 << 20, 12}, // HeapPeakBounded shape
		{8 << 20, 10 << 20, 1},    // 8MiB / (2×3.75+0.5)MiB = 1
		{600 << 10, 10 << 20, 1},  // sub-block budget still admits ONE reader
	} {
		b := NewInflightBodyBudget(tc.limit)
		ReadBodyWithBudget(tc.maxSize, b, 0) // construction registers the ceiling
		if got := int64(cap(b.unknownSlots())); got != tc.wantK {
			t.Fatalf("slots for limit=%d maxSize=%d: got %d want %d", tc.limit, tc.maxSize, got, tc.wantK)
		}
	}
}
