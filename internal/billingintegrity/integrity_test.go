package billingintegrity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// r34: an interleaved loss must RESET the consecutive-success chain —
// "5 consecutive" previously survived losses interleaved between successes,
// releasing the block on durability that was failing intermittently.
func TestLossPolicy_InterleavedLossResetsConsecutive(t *testing.T) {
	ResetForTest()
	Configure(PolicyBlock)

	ReportLoss("db write failed; event not WAL-backed", "evt-1")
	FastForwardHoldForTest()
	for i := 0; i < 4; i++ {
		ReportDurableSuccess()
	}
	// A loss lands between the 4th and 5th success: the chain resets.
	ReportLoss("db write failed; event not WAL-backed", "evt-2")
	for i := 0; i < 4; i++ {
		ReportDurableSuccess()
	}
	if !Blocked() {
		t.Fatal("block released despite a loss interleaved into the success chain (consecutive not enforced)")
	}
	ReportDurableSuccess() // 5 GENUINE consecutive after the loss
	if Blocked() {
		t.Fatal("block must release after 5 genuine consecutive successes")
	}
}

// r34: the active prober releases an IDLE block (empty WAL, nothing in
// flight — no organic durable success would ever arrive).
func TestProber_ReleasesIdleBlock(t *testing.T) {
	ResetForTest()
	Configure(PolicyBlock)
	probeInterval = 20 * time.Millisecond
	t.Cleanup(func() { probeInterval = 5 * time.Second })

	ReportLoss("WAL unavailable; synchronous db write failed", "evt-1")
	FastForwardHoldForTest()
	SetProber(func() error { return nil })
	StartProber()

	deadline := time.Now().Add(3 * time.Second)
	for Blocked() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Blocked() {
		t.Fatal("idle block never released: active probe did not count toward recovery")
	}
}

// r34: a FAILING probe holds the block and resets the chain — probing must
// not fake recovery.
func TestProber_FailureHoldsBlockAndResetsChain(t *testing.T) {
	ResetForTest()
	Configure(PolicyBlock)
	probeInterval = 20 * time.Millisecond
	t.Cleanup(func() { probeInterval = 5 * time.Second })

	ReportLoss("db write failed; event not WAL-backed", "evt-1")
	FastForwardHoldForTest()
	failing := true
	SetProber(func() error {
		if failing {
			return errors.New("probe: db write failed")
		}
		return nil
	})
	StartProber()

	// Failing probes keep the block held despite anything else.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !Blocked() {
		t.Fatal("block released while the probe was failing")
	}
	// Probe succeeds again: 5 consecutive probes + hold release it.
	failing = false
	deadline = time.Now().Add(3 * time.Second)
	for Blocked() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if Blocked() {
		t.Fatal("block not released after probes recovered")
	}
}

// r34: a process restart must NOT silently clear an engaged block — the
// state file re-engages it (fresh hold; recovery must re-prove).
func TestStateFile_BlockSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "billing-integrity.state")

	ResetForTest()
	SetStateFile(path)
	t.Cleanup(func() { SetStateFile(""); ResetForTest() })
	Configure(PolicyBlock)

	ReportLoss("db write failed; event not WAL-backed", "evt-1")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("engage must persist the state file: %v", err)
	}

	// Simulate the restart: in-process state wiped, file on disk.
	ResetProcessForTest()
	if Blocked() {
		t.Fatal("precondition: in-process state cleared")
	}
	Configure(PolicyBlock) // boot path
	if !Blocked() {
		t.Fatal("restart silently cleared the engaged block")
	}
	_, _, reason := BlockInfo()
	if !strings.Contains(reason, "restored after restart") {
		t.Fatalf("restore reason = %q, want restored-after-restart marker", reason)
	}

	// Recovery removes the file.
	FastForwardHoldForTest()
	for i := 0; i < 5; i++ {
		ReportDurableSuccess()
	}
	if Blocked() {
		t.Fatal("release failed after consecutive successes")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file must be removed on release, got %v", err)
	}
}
