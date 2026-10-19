// Package billingintegrity implements the user-decided (2026-10-19)
// ledger-integrity-first policy for the DB+WAL double failure: when a
// billing row can be persisted NOWHERE (DB write failed AND the WAL could
// not take it), the gateway STOPS ACCEPTING new billable requests and
// alerts, until durability demonstrably recovers.
//
// Persistence guarantee & residual risk (stated, not hand-waved): rows are
// durable when the WAL fsync-before-ack holds; rows already in the WAL are
// owned by replay. The block protects FUTURE requests only — rows lost in
// the window before the block engaged are visible as the
// cl_usage_rows_lost_total delta over the incident and are NOT recoverable;
// in-flight responses that already streamed keep their WAL/lost state as it
// was at completion. Zero-loss is claimed for nothing.
package billingintegrity

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// PolicyCount is the legacy behavior: losses counted, traffic continues.
	PolicyCount = "count"
	// PolicyBlock is the decided default: on a real loss, stop accepting
	// new billable requests until recovery conditions hold.
	PolicyBlock = "block"
)

// recoveryConsecutive and recoveryMinAge define the release condition:
// N consecutive durable successes AND at least this long since the block
// engaged (avoids flapping on a single lucky write).
const (
	recoveryConsecutive = 5
	recoveryMinAge      = 30 * time.Second
)

var (
	mode          atomic.Value // string
	blocked       atomic.Bool
	blockedSince  atomic.Int64 // unix seconds
	blockedReason atomic.Value // string
	consec        atomic.Int64
)

var blockedGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cl_billing_integrity_blocked",
	Help: "1 = ledger persistence failed (DB+WAL) and new billable requests are rejected until recovery (policy usage.loss_policy=block).",
})

// Configure installs the policy ("count" or "block"; default block).
func Configure(policy string) {
	switch policy {
	case PolicyCount:
		mode.Store(PolicyCount)
	default:
		mode.Store(PolicyBlock)
	}
}

func currentMode() string {
	if m, ok := mode.Load().(string); ok {
		return m
	}
	return PolicyBlock
}

// ReportLoss records a REAL loss (no durable path held the row). In block
// mode it engages the block; in count mode it only counts (legacy).
func ReportLoss(reason, eventID string) {
	if currentMode() != PolicyBlock {
		return
	}
	if blocked.CompareAndSwap(false, true) {
		blockedSince.Store(time.Now().Unix())
		blockedReason.Store(reason)
		blockedGauge.Set(1)
		slog.Error("BILLING INTEGRITY BLOCK ENGAGED: a ledger row was lost (DB and WAL both failed); new billable requests are rejected until durability recovers",
			"reason", reason, "event_id", eventID, "policy", PolicyBlock)
	}
}

// ReportDurableSuccess records a durably persisted row (WAL-held event
// committed to DB, or a synchronous commit). Consecutive successes release
// the block after recoveryConsecutive successes and recoveryMinAge.
func ReportDurableSuccess() {
	if !blocked.Load() {
		return
	}
	if consec.Add(1) >= recoveryConsecutive && time.Since(time.Unix(blockedSince.Load(), 0)) >= recoveryMinAge {
		if blocked.CompareAndSwap(true, false) {
			consec.Store(0)
			blockedGauge.Set(0)
			slog.Error("BILLING INTEGRITY BLOCK RELEASED: durability recovered (consecutive durable commits observed)",
				"held_for", time.Since(time.Unix(blockedSince.Load(), 0)).Round(time.Second).String())
		}
	}
}

// Blocked reports whether new billable requests must be rejected.
func Blocked() bool { return blocked.Load() }

// BlockInfo returns the engagement state for logs and tests.
func BlockInfo() (bool, time.Time, string) {
	var since time.Time
	var reason string
	if s := blockedSince.Load(); s != 0 {
		since = time.Unix(s, 0)
	}
	if r, ok := blockedReason.Load().(string); ok {
		reason = r
	}
	return blocked.Load(), since, reason
}

// ResetForTest clears the state (tests only).
func ResetForTest() {
	blocked.Store(false)
	blockedSince.Store(0)
	consec.Store(0)
	blockedGauge.Set(0)
}

// FastForwardHoldForTest compresses the minimum hold time (tests only): the
// recovery threshold's consecutive-success logic is preserved.
func FastForwardHoldForTest() {
	if blocked.Load() {
		blockedSince.Store(time.Now().Add(-2 * recoveryMinAge).Unix())
	}
}
