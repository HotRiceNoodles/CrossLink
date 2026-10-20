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
//
// Recovery (r34 hardening): the gate rejects new billable traffic, so
// organic durable successes may STOP ENTIRELY (empty WAL backlog, nothing
// in flight) — an unprobed block would never release. Release therefore
// counts (a) WAL-replay flush successes, (b) real pipeline commits, and
// (c) an ACTIVE durability probe every probeInterval while blocked: a
// zero-cost probe row written through the SAME WAL→DB path. A probe failure
// resets the consecutive counter, so "N consecutive" is genuinely
// consecutive (r34 fix: interleaved losses used to leave the counter
// standing). A restart does NOT silently clear an engaged block: the state
// file re-engages it until probes re-prove durability.
package billingintegrity

import (
	"encoding/json"
	"log/slog"
	"os"
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
// engaged (avoids flapping on a single lucky write). "Consecutive" is
// enforced by resetting the counter on EVERY loss and EVERY failed probe.
const (
	recoveryConsecutive = 5
	recoveryMinAge      = 30 * time.Second
)

// probeInterval paces the active durability probe while blocked (var for
// tests).
var probeInterval = 5 * time.Second

var (
	mode          atomic.Value // string
	blocked       atomic.Bool
	blockedSince  atomic.Int64 // unix seconds
	blockedReason atomic.Value // string
	consec        atomic.Int64
)

// probeFn is the injected durability probe (app wiring: a zero-cost row
// through the real WAL→DB pipeline). nil disables active probing.
var probeFn atomic.Value // func() error

// stateFile persists the engaged block across process restarts (r34: a
// crash/restart must not silently clear an unfixed block). Empty = off.
var stateFile atomic.Value // string

var blockedGauge = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cl_billing_integrity_blocked",
	Help: "1 = ledger persistence failed (DB+WAL) and new billable requests are rejected until recovery (policy usage.loss_policy=block).",
})

// Configure installs the policy ("count" or "block"; default block). If a
// state file was configured and still exists, an engaged block is RESTORED
// (fresh hold timer — recovery must re-prove itself after a restart).
func Configure(policy string) {
	switch policy {
	case PolicyCount:
		mode.Store(PolicyCount)
	default:
		mode.Store(PolicyBlock)
	}
	restoreFromStateFile()
}

// SetStateFile arms block-state persistence at path. Call before Configure.
// The file records {reason, since}; engage writes it, release removes it.
func SetStateFile(path string) {
	stateFile.Store(path)
}

// SetProber installs the active durability probe run every probeInterval
// while blocked (app wiring: handler.ProbeUsageDurability). nil = off.
func SetProber(fn func() error) {
	if fn == nil {
		probeFn.Store((func() error)(nil))
		return
	}
	probeFn.Store(fn)
}

// StartProber launches the background probe loop (idempotent). The app calls
// it once at boot; the loop only acts while blocked.
func StartProber() {
	go func() {
		t := time.NewTicker(probeInterval)
		defer t.Stop()
		for range t.C {
			if !Blocked() {
				continue
			}
			fn, _ := probeFn.Load().(func() error)
			if fn == nil {
				continue
			}
			if err := fn(); err != nil {
				// Durability NOT proven: a failing probe breaks the
				// consecutive-success chain exactly like a real loss.
				consec.Store(0)
				slog.Warn("billing integrity probe FAILED (block held)",
					"error", err, "consecutive_reset", true)
				continue
			}
			ReportDurableSuccess()
		}
	}()
}

// ReportLoss records a REAL loss (no durable path held the row). In block
// mode it engages the block; in count mode it only counts (legacy). Every
// loss RESETS the consecutive-success counter — otherwise "N consecutive"
// could span interleaved losses (r34 fix).
func ReportLoss(reason, eventID string) {
	if currentMode() != PolicyBlock {
		return
	}
	consec.Store(0)
	if blocked.CompareAndSwap(false, true) {
		blockedSince.Store(time.Now().Unix())
		blockedReason.Store(reason)
		blockedGauge.Set(1)
		persistState(reason)
		slog.Error("BILLING INTEGRITY BLOCK ENGAGED: a ledger row was lost (DB and WAL both failed); new billable requests are rejected until durability recovers",
			"reason", reason, "event_id", eventID, "policy", PolicyBlock)
	}
}

// ReportDurableSuccess records a durably persisted row (WAL-held event
// committed to DB, a synchronous commit, a replay flush, or a successful
// probe). Consecutive successes release the block after
// recoveryConsecutive successes and recoveryMinAge.
func ReportDurableSuccess() {
	if !blocked.Load() {
		return
	}
	if consec.Add(1) >= recoveryConsecutive && time.Since(time.Unix(blockedSince.Load(), 0)) >= recoveryMinAge {
		if blocked.CompareAndSwap(true, false) {
			consec.Store(0)
			blockedGauge.Set(0)
			clearStateFile()
			slog.Error("BILLING INTEGRITY BLOCK RELEASED: durability recovered (consecutive durable commits/probes observed)",
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

type blockState struct {
	Reason string `json:"reason"`
	Since  int64  `json:"since"`
}

func statePath() string {
	p, _ := stateFile.Load().(string)
	return p
}

func persistState(reason string) {
	p := statePath()
	if p == "" {
		return
	}
	st := blockState{Reason: reason, Since: time.Now().Unix()}
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	if err := os.WriteFile(p, b, 0600); err != nil {
		slog.Warn("billing integrity: block state file write failed (block still active in-process; a restart would NOT restore it)",
			"path", p, "error", err)
	}
}

func clearStateFile() {
	p := statePath()
	if p == "" {
		return
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		slog.Warn("billing integrity: block state file remove failed", "path", p, "error", err)
	}
}

// restoreFromStateFile re-engages a block whose state file survived a
// restart. The hold timer restarts fresh: recovery must re-prove with 5
// consecutive durable successes plus the minimum hold after boot.
func restoreFromStateFile() {
	p := statePath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("billing integrity: state file unreadable", "path", p, "error", err)
		}
		return
	}
	var st blockState
	if err := json.Unmarshal(b, &st); err != nil {
		slog.Warn("billing integrity: state file corrupt; ignoring", "path", p, "error", err)
		return
	}
	if blocked.CompareAndSwap(false, true) {
		blockedSince.Store(time.Now().Unix())
		blockedReason.Store(st.Reason + " (restored after restart)")
		consec.Store(0)
		blockedGauge.Set(1)
		slog.Error("BILLING INTEGRITY BLOCK RESTORED from state file: durability was NOT proven before the previous process ended; block re-engaged until recovery re-proves",
			"path", p, "original_reason", st.Reason,
			"original_since", time.Unix(st.Since, 0).Format(time.RFC3339))
	}
}

func currentMode() string {
	if m, ok := mode.Load().(string); ok {
		return m
	}
	return PolicyBlock
}

// ResetForTest clears the state (tests only).
func ResetForTest() {
	blocked.Store(false)
	blockedSince.Store(0)
	consec.Store(0)
	blockedGauge.Set(0)
	clearStateFile()
}

// ResetProcessForTest wipes ONLY the in-process state, keeping the state
// file on disk — simulating a process restart (tests only).
func ResetProcessForTest() {
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
