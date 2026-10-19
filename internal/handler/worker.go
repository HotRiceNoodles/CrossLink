package handler

import (
	"context"
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

// usageRowsLostTotal counts billing rows that could not be persisted through
// ANY path: the DB write failed AND the spool compensation also failed (nil,
// at cap, or disk error). Every increment is a row missing from usage_logs —
// the budget-calibration ground truth — so it must be alerted on.
var usageRowsLostTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "cl_usage_rows_lost_total",
	Help: "Usage billing rows lost: DB write failed and spool compensation was unavailable.",
})

// SetUsageWorkers initializes the package-level usage worker pool.
func SetUsageWorkers(p *worker.Pool) {
	UsageWorkers = p
}

// SetUsageSpool arms the disk spill fallback for usage rows.
func SetUsageSpool(s *UsageSpool) {
	usageSpool = s
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
// synchronous DB write as reliable admission: committed → done; failed → a
// counted real loss (never a silent one).
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
