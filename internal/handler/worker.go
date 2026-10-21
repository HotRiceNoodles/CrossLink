package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/crosslink/internal/billingintegrity"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/worker"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// UsageWorkers is a package-level worker pool for async usage logging.
// Set once during app initialization via SetUsageWorkers.
var UsageWorkers *worker.Pool

// usageSpool is the disk persistence fallback for usage rows the pool cannot
// accept (queue + overflow both full). Set via SetUsageSpool; nil disables
// spooling and the spill path degrades to a synchronous write.
var usageSpool *UsageSpool

// usageSpoolEmergency is the second-tier overflow WAL (r53 ③): it takes a
// row ONLY when the main WAL could not (cap/disk) AND the synchronous DB
// commit failed — the exact DB+WAL double-failure point that used to drop
// in-flight rows (counted, then the integrity block). While it holds the
// row it is durable on disk and its replay loop owns it; a real loss now
// requires the disk itself to fail. nil = not armed (spooling disabled).
var usageSpoolEmergency *UsageSpool

// usageRowsLostTotal counts billing rows that could not be persisted through
// ANY path: the DB write failed AND the spool compensation also failed (nil,
// at cap, or disk error). Every increment is a row missing from usage_logs —
// the budget-calibration ground truth — so it must be alerted on.
var usageRowsLostTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_usage_rows_lost_total",
	Help: "Usage billing rows lost: DB write failed and spool compensation was unavailable.",
})

// usageEmergencyHeldTotal counts rows the emergency overflow WAL took at the
// double-failure point (main WAL unavailable + DB write failed). These are
// NOT losses — they are durable-pending; replay flushes them once the DB
// recovers. A sustained climb means the main WAL cap is undersized for the
// outage (or the DB is down for longer than the caps cover).
var usageEmergencyHeldTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_usage_rows_emergency_held_total",
	Help: "Usage rows held by the emergency overflow WAL (main WAL unavailable AND db write failed); durable-pending, drained by replay.",
})

// SetUsageWorkers initializes the package-level usage worker pool.
func SetUsageWorkers(p *worker.Pool) {
	UsageWorkers = p
}

// SetUsageSpool arms the disk spill fallback for usage rows.
func SetUsageSpool(s *UsageSpool) {
	usageSpool = s
}

// SetUsageSpoolEmergency arms the second-tier overflow WAL.
func SetUsageSpoolEmergency(s *UsageSpool) {
	usageSpoolEmergency = s
}

// emergencyHold is the last durable line of defense at a double-failure
// point: it parks the row in the emergency WAL instead of counting a real
// loss. Returns true when the row is fsync-durable there (replay owns it).
func emergencyHold(entry *service.UsageEntry) bool {
	if usageSpoolEmergency == nil {
		return false
	}
	if !usageSpoolEmergency.Write(entry) {
		return false
	}
	usageEmergencyHeldTotal.Inc()
	slog.Warn("usage row EMERGENCY-held: main WAL unavailable and db write failed; row is durable-pending in the emergency WAL",
		"event_id", entry.EventID)
	checkEmergencyHighWater()
	return true
}

// emergencyHighWaterReserve is the in-flight row budget the pre-loss block
// must cover once engaged: after the block, new billable requests are
// rejected, so at most concurrency_limit already-admitted requests can
// still complete and each contributes one WAL line (typical ~1-2KB; wide
// content-logged rows more — operators size the cap accordingly, see the
// deployment support table). 64MiB covers ~32K typical rows; when the cap
// is smaller the reserve is half of it.
func emergencyHighWaterReserve(cap int64) int64 {
	const r = int64(64 << 20)
	if cap <= 0 {
		return 0
	}
	if cap/2 < r {
		return cap / 2
	}
	return r
}

// checkEmergencyHighWater engages the billing-integrity block BEFORE the
// emergency WAL can fill (r58 review): rows stop being admitted while the
// remaining headroom still provably covers the bounded in-flight set. A
// burst larger than the reserve between high-water and cap still lands in
// the counted loss path (loud, block engages there too) — the boundary is
// narrowed, and the residual is stated rather than claimed away.
func checkEmergencyHighWater() {
	cap := usageSpoolEmergency.Cap()
	reserve := emergencyHighWaterReserve(cap)
	if reserve == 0 || cap <= 0 {
		return
	}
	if used := usageSpoolEmergency.Size(); used >= cap-reserve {
		billingintegrity.Engage(fmt.Sprintf("emergency WAL at high-water: %d/%d bytes (reserve %d) while db write failed; blocking new billable requests before both WALs can fill", used, cap, reserve))
	}
}

// ProbeUsageDurability exercises the EXACT dual persistence path a billing
// row depends on: WAL (spool) accept + synchronous DB commit (billingintegrity
// active probe, r34). The probe row is zero-cost and route-tagged
// (integrity_probe) — auditable in usage_logs, contributes 0 to every SUM.
// Returns nil only when both halves held; the caller counts it as a durable
// success (or resets its consecutive chain on failure).
func ProbeUsageDurability(log usageEntryLogger) error {
	entry := &service.UsageEntry{
		// EventID must stay a bare 36-char UUID: usage_logs.event_id is
		// varchar(36) (r34 e2e finding — a prefixed id exceeded the column
		// and the probe itself failed on Postgres). The probe marker is the
		// route_type below.
		EventID:    uuid.New().String(),
		RouteType:  "integrity_probe",
		StatusCode: 200,
		Currency:   "USD",
	}
	if usageSpool != nil {
		if !usageSpool.Write(entry) {
			return errors.New("probe: WAL (spool) rejected the probe row")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageWriteTimeout)
	defer cancel()
	if err := log.LogChecked(ctx, entry); err != nil {
		return fmt.Errorf("probe: db write failed: %w", err)
	}
	if usageSpool != nil {
		usageSpool.Ack(entry.EventID)
	}
	return nil
}

// usageEntryLogger is the subset of the usage service the billing path
// needs: the returned error IS the ack signal (nil = durably stored).
type usageEntryLogger interface {
	LogChecked(ctx context.Context, entry *service.UsageEntry) error
}

// submitUsage submits an opaque task to the worker pool (legacy helper for
// paths that do not carry a billing row).
func submitUsage(fn func()) {
	if UsageWorkers != nil {
		UsageWorkers.Submit(fn)
		return
	}
	go fn()
}

// usageWriteTimeout bounds every billing DB write (acceptance blocker 1:
// runUsageTask used context.Background — a hung DB pinned the worker and the
// synchronous fallback forever). The WAL still holds the event, so a timeout
// merely hands the row to the replay loop.
const usageWriteTimeout = 30 * time.Second

// runUsageTask persists one billing row. walHeld reports whether THIS event
// was durably written to the WAL before enqueue — on DB failure the row is
// only safe when walHeld is true (replay owns it); otherwise it is a real,
// counted loss (acceptance round 2: the first version checked the spool
// pointer, which is wrong when the WAL write itself failed at cap/disk).
func runUsageTask(log usageEntryLogger, entry *service.UsageEntry, walHeld bool) {
	ctx, cancel := context.WithTimeout(context.Background(), usageWriteTimeout)
	defer cancel()
	if err := log.LogChecked(ctx, entry); err != nil {
		if !walHeld {
			if emergencyHold(entry) {
				return
			}
			usageRowsLostTotal.Inc()
			billingintegrity.ReportLoss("db write failed; event not WAL-backed", entry.EventID)
			slog.Error("usage row LOST: db write failed and this event was NOT WAL-backed",
				"event_id", entry.EventID, "error", err)
			return
		}
		slog.Warn("usage db write failed; event stays in WAL for replay",
			"event_id", entry.EventID, "error", err)
		return
	}
	// Committed: ack-driven compaction strips the WAL line soon (2s ticker /
	// threshold) — the WAL plateaus under sustained load instead of waiting
	// for the 30s replay.
	usageSpool.Ack(entry.EventID)
	billingintegrity.ReportDurableSuccess()
}

// submitUsageEntry submits one billing row. Protocol (acceptance rounds
// 1+2): WAL-before-enqueue (fsync durable BEFORE any in-memory handoff) +
// per-event walHeld + ack-driven compaction. When the WAL itself is
// unavailable (cap reached / disk error), the caller does a bounded
// synchronous DB write as reliable admission: committed → done; failed →
// the emergency overflow WAL holds the row (r53 ③); only when that fails
// too (disk) is it a counted real loss (never a silent one).
func submitUsageEntry(log usageEntryLogger, entry *service.UsageEntry) {
	submitUsageEntryFunc(log, entry, nil)
}

func submitUsageEntryFunc(log usageEntryLogger, entry *service.UsageEntry, prepare func()) {
	if prepare != nil {
		prepare()
	}
	if entry.EventID == "" {
		entry.EventID = uuid.New().String()
	}
	walHeld := usageSpool.Write(entry)
	if !walHeld && usageSpool != nil {
		// WAL armed but could not take the event (cap/disk). Reliable
		// admission fallback: bounded synchronous commit NOW; failure is a
		// counted loss. Do not enqueue — the event is already settled.
		ctx, cancel := context.WithTimeout(context.Background(), usageWriteTimeout)
		err := log.LogChecked(ctx, entry)
		cancel()
		if err != nil {
			if emergencyHold(entry) {
				return
			}
			usageRowsLostTotal.Inc()
			billingintegrity.ReportLoss("WAL unavailable; synchronous db write failed", entry.EventID)
			slog.Error("usage row LOST: WAL unavailable and synchronous db write failed",
				"event_id", entry.EventID, "error", err)
			return
		}
		billingintegrity.ReportDurableSuccess()
		return
	}
	fn := func() { runUsageTask(log, entry, walHeld) }
	if UsageWorkers != nil {
		if UsageWorkers.TrySubmit(fn) {
			return
		}
		if walHeld {
			return // pool saturated: the WAL holds the event, replay owns it
		}
		fn() // no WAL (disabled): synchronous attempt is the last resort
		return
	}
	go fn()
}
