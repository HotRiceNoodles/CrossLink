package billingintegrity

import (
	"testing"
)

// Policy matrix: count never blocks; block engages on the FIRST real loss
// and releases only after consecutive durable successes AND the minimum
// hold time (recovery condition explicit).
func TestLossPolicy_BlockEngageAndRecover(t *testing.T) {
	ResetForTest()
	Configure(PolicyBlock)

	if Blocked() {
		t.Fatal("must start unblocked")
	}
	ReportLoss("db write failed; event not WAL-backed", "evt-1")
	if !Blocked() {
		t.Fatal("block must engage on first real loss")
	}
	_, since, reason := BlockInfo()
	if reason == "" || since.IsZero() {
		t.Fatalf("block info incomplete: since=%v reason=%q", since, reason)
	}

	// Compress the minimum hold for the test, then verify the consecutive
	// threshold gates the release.
	FastForwardHoldForTest()
	for i := 0; i < 4; i++ {
		ReportDurableSuccess()
	}
	if !Blocked() {
		t.Fatal("released before the consecutive threshold")
	}
	ReportDurableSuccess()
	if Blocked() {
		t.Fatal("must release after N consecutive durable successes + hold time")
	}

	// Re-engagement works after a subsequent loss.
	ReportLoss("WAL unavailable; synchronous db write failed", "evt-2")
	if !Blocked() {
		t.Fatal("re-engagement failed")
	}
}

// count policy (legacy) never engages the block.
func TestLossPolicy_CountNeverBlocks(t *testing.T) {
	ResetForTest()
	Configure(PolicyCount)
	ReportLoss("db write failed; event not WAL-backed", "evt-3")
	if Blocked() {
		t.Fatal("count policy must never block")
	}
}
