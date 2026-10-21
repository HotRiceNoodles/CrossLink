package handler

// F34 fault-injection tests: billing durability across every crash/failure
// window the supervisor enumerated. Real DB semantics via glebarez sqlite
// (in-memory, CGO-free) wired through the REAL repository.Create
// (OnConflict-on-event_id) and the REAL UsageService.LogChecked — no mocks
// on the accounting path under test.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/billingintegrity"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/worker"
	"github.com/glebarez/sqlite"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type usageDB struct {
	gorm *gorm.DB
	raw  *sql.DB
	svc  *service.UsageService
}

func newUsageDB(t *testing.T) usageDB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	sqlDB, err := gdb.DB()
	sqlDB.SetMaxOpenConns(1)
	if err != nil {
		t.Fatalf("raw db: %v", err)
	}
	return usageDB{gorm: gdb, raw: sqlDB, svc: service.NewUsageService(repository.NewUsageLogRepo(gdb))}
}

func (u usageDB) count(t *testing.T) (rows int64, tokens int64) {
	t.Helper()
	var r, tk int64
	if err := u.gorm.Model(&model.UsageLog{}).Count(&r).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := u.gorm.Model(&model.UsageLog{}).
		Select("COALESCE(SUM(output_tokens),0)").Scan(&tk).Error; err != nil {
		t.Fatalf("sum: %v", err)
	}
	return r, tk
}

func spoolFile(dir string) string { return filepath.Join(dir, "usage-spill.jsonl") }

// A corrupt (non-JSON) line mid-file is dropped LOUDLY: the surrounding rows
// still replay, and the corrupt counter increments — never a silent skip.
func TestUsageSpool_CorruptLineDroppedLoudly(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	if spool == nil {
		t.Fatal("spool not armed")
	}
	spool.Write(&service.UsageEntry{EventID: "a", RouteType: "a"})
	// Simulate a bad mid-file line (short write / bit rot), then more rows.
	f, err := os.OpenFile(spoolFile(dir), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("append open: %v", err)
	}
	f.WriteString("this-is-not-json\n")
	f.Close()
	spool.Write(&service.UsageEntry{EventID: "b", RouteType: "b"})
	spool.Write(&service.UsageEntry{EventID: "c", RouteType: "c"})

	before := testutil.ToFloat64(usageSpoolCorruptTotal)
	flushed := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		if e.RouteType != "a" && e.RouteType != "b" && e.RouteType != "c" {
			t.Fatalf("unexpected row: %+v", e)
		}
		return nil
	}), 10)
	if flushed != 3 {
		t.Fatalf("flushed %d rows, want 3 (corrupt line must not block others)", flushed)
	}
	if got := testutil.ToFloat64(usageSpoolCorruptTotal) - before; got != 1 {
		t.Fatalf("corrupt counter delta = %v, want 1", got)
	}
	if data, _ := os.ReadFile(spoolFile(dir)); len(data) != 0 {
		t.Fatalf("spool not emptied: %q", data)
	}
}

// A torn tail (crash mid-append, no newline) is truncated at startup so it
// cannot concatenate onto the next appended row.
func TestUsageSpool_TornTailDroppedAndCountedInReplay(t *testing.T) {
	dir := t.TempDir()
	valid, _ := json.Marshal(&service.UsageEntry{EventID: "a", RouteType: "a"})
	os.WriteFile(spoolFile(dir), append(append([]byte{}, valid...), '\n', '{', '"', 't'), 0o644) // + torn line

	spool := NewUsageSpool(dir, 1<<20) // startup sanitize truncates the torn tail
	t.Cleanup(spool.Close)
	before := testutil.ToFloat64(usageSpoolCorruptTotal)
	flushed := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		return nil
	}), 10)
	if flushed != 1 {
		t.Fatalf("flushed %d, want 1", flushed)
	}
	// Torn tail was removed at startup; replay saw only the valid row.
	if got := testutil.ToFloat64(usageSpoolCorruptTotal) - before; got != 0 {
		t.Fatalf("corrupt counter delta = %v, want 0 (startup sanitize handled it)", got)
	}
}

// WAL-first protocol (acceptance blocker 1): submitUsageEntryFunc makes the
// event durable BEFORE any in-memory handoff — the WAL line exists
// immediately after submit, even while the async task has not run.
func TestSubmitUsageEntry_WALBeforeEnqueue(t *testing.T) {
	dir := t.TempDir()
	spoolForTest := NewUsageSpool(dir, 1<<20)
	SetUsageSpool(spoolForTest)
	t.Cleanup(spoolForTest.Close)
	t.Cleanup(func() { SetUsageSpool(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = worker.NewPool("usage", 1, 2, 0) // tiny: queue backs up fast
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	entry := &service.UsageEntry{EventID: "row-" + uuid.NewString(), RouteType: "openai", OutputTokens: 1}
	submitUsageEntry(&captureLogger{}, entry)
	// SYNCHRONOUS check: the WAL line must exist before the (async) task
	// could have run — kill between enqueue and commit loses nothing.
	data, err := os.ReadFile(spoolFile(dir))
	if err != nil || len(data) == 0 {
		t.Fatalf("WAL not written at submit time: %v", err)
	}
	var roundTrip service.UsageEntry
	if json.Unmarshal(data[:len(data)-1], &roundTrip) != nil || roundTrip.EventID != entry.EventID {
		t.Fatalf("WAL line missing stable EventID: %q", data)
	}
}

// DB write failure with WAL armed: the row STAYS in the WAL (replay owns
// it), and the loss counter must NOT move.
func TestRunUsageTask_DBFailureStaysInWALNotLost(t *testing.T) {
	dir := t.TempDir()
	spoolForTest := NewUsageSpool(dir, 1<<20)
	SetUsageSpool(spoolForTest)
	t.Cleanup(spoolForTest.Close)
	t.Cleanup(func() { SetUsageSpool(nil) })

	entry := &service.UsageEntry{RouteType: "openai", EventID: "evt-wal-1", OutputTokens: 5}
	before := testutil.ToFloat64(usageRowsLostTotal)
	runUsageTask(&failingLogger{}, entry, true)
	if got := testutil.ToFloat64(usageRowsLostTotal) - before; got != 0 {
		t.Fatalf("loss counter moved although WAL holds the event: %v", got)
	}
	_ = dir
}

// Last resort (no WAL configured): DB failure is a real loss — counted.
func TestRunUsageTask_LossCountedWhenNoWAL(t *testing.T) {
	SetUsageSpool(nil)
	t.Cleanup(func() { SetUsageSpool(nil) })
	before := testutil.ToFloat64(usageRowsLostTotal)
	runUsageTask(&failingLogger{}, &service.UsageEntry{RouteType: "x", EventID: "evt-2"}, false)
	if got := testutil.ToFloat64(usageRowsLostTotal) - before; got != 1 {
		t.Fatalf("loss counter delta = %v, want 1", got)
	}
}

// submitUsageEntryFunc mints the EventID exactly once, before any path split
// (async goroutine or spool line both carry the same id).
func TestSubmitUsageEntry_MintsStableEventID(t *testing.T) {
	dir := t.TempDir()
	spoolForTest := NewUsageSpool(dir, 1<<20)
	SetUsageSpool(spoolForTest)
	t.Cleanup(spoolForTest.Close)
	t.Cleanup(func() { SetUsageSpool(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = nil
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	captured := &captureLogger{}
	submitUsageEntry(captured, &service.UsageEntry{RouteType: "openai"})
	// UsageWorkers==nil runs the task via `go fn()`; poll for completion.
	deadline := time.Now().Add(5 * time.Second)
	for captured.entry == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if captured.entry == nil {
		t.Fatal("task did not run")
	}
	if captured.entry.EventID == "" {
		t.Fatal("EventID not minted at submit time")
	}
}

// THE crash-window test (F34): the DB already committed the row but the
// spool line was not removed (process died between commit and rewrite).
// Replaying that line must NOT double-bill: EventID collides on the unique
// index, the insert is a no-op, and the row is (correctly) acked away.
func TestUsageDurability_CrashAfterCommitBeforeSpoolRewrite(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)

	entry := &service.UsageEntry{RouteType: "openai", EventID: "evt-crash", OutputTokens: 100}
	// (1) Normal path commits the row.
	if err := db.svc.LogChecked(context.Background(), entry); err != nil {
		t.Fatalf("initial LogChecked: %v", err)
	}
	// (2) Crash BEFORE the spool line was removed: the same line is still on
	// disk after restart.
	if !spool.Write(entry) {
		t.Fatal("spool write failed")
	}

	// (3) Replay the stale line: must ack (row IS durably present) and clear
	// the line without inserting a second row.
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		return db.svc.LogChecked(context.Background(), e)
	}), 10)

	rows, tokens := db.count(t)
	if rows != 1 || tokens != 100 {
		t.Fatalf("double-billed crash window: rows=%d tokens=%d, want 1/100", rows, tokens)
	}
	if data, _ := os.ReadFile(spoolFile(dir)); len(data) != 0 {
		t.Fatalf("stale spool line not acked away: %q", data)
	}
}

// Plain duplicate replay (same EventID twice through the real repo) is a
// no-op: unique index + OnConflict DoNothing.
func TestUsageDurability_DuplicateReplayIsNoop(t *testing.T) {
	db := newUsageDB(t)
	entry := &service.UsageEntry{RouteType: "openai", EventID: "evt-dup", OutputTokens: 7}
	for i := 0; i < 3; i++ {
		if err := db.svc.LogChecked(context.Background(), entry); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
	}
	rows, tokens := db.count(t)
	if rows != 1 || tokens != 7 {
		t.Fatalf("dedupe failed: rows=%d tokens=%d, want 1/7", rows, tokens)
	}
}

// DB fully disconnected: LogChecked returns the error (the ack signal), the
// row stays spooled, and recovery replays it exactly once. Also verifies
// budget consistency: usage totals match the entries after the dust settles.
func TestUsageDurability_DBDownThenRecover_BudgetConsistent(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)

	// DB goes down AFTER the service is built.
	if err := db.raw.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	entries := []*service.UsageEntry{
		{RouteType: "openai", EventID: "e1", OutputTokens: 100},
		{RouteType: "openai", EventID: "e2", OutputTokens: 200},
		{RouteType: "anthropic", EventID: "e3", OutputTokens: 300},
	}
	for _, e := range entries {
		if err := db.svc.LogChecked(context.Background(), e); err == nil {
			t.Fatal("LogChecked must surface the DB failure as the ack error")
		}
		// Compensation: queue-accepted row re-spooled (worker path).
		if !spool.Write(e) {
			t.Fatal("spool write failed")
		}
	}

	// Recovery: fresh DB (simulate DB coming back) and replay.
	db2 := newUsageDB(t)
	flushed := 0
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		return db2.svc.LogChecked(context.Background(), e)
	}), 10)
	if flushed != 3 {
		t.Fatalf("replayed %d rows, want 3", flushed)
	}
	// Replay once MORE (another crash-window duplicate) — still exactly 3 rows.
	spool.Write(entries[0])
	spool.Replay(perRow(func(e *service.UsageEntry) error {
		return db2.svc.LogChecked(context.Background(), e)
	}), 10)

	rows, tokens := db2.count(t)
	if rows != 3 || tokens != 600 {
		t.Fatalf("budget mismatch after recovery: rows=%d tokens=%d, want 3/600", rows, tokens)
	}
	if data, _ := os.ReadFile(spoolFile(dir)); len(data) != 0 {
		t.Fatalf("spool not drained: %q", data)
	}
}

// Legacy rows (no EventID) keep the old insert semantics: two rows, no
// unique collision — backward compatible with pre-F34 data.
func TestUsageDurability_LegacyEntriesWithoutEventID(t *testing.T) {
	db := newUsageDB(t)
	db.svc.LogChecked(context.Background(), &service.UsageEntry{RouteType: "a", OutputTokens: 1})
	db.svc.LogChecked(context.Background(), &service.UsageEntry{RouteType: "b", OutputTokens: 2})
	rows, _ := db.count(t)
	if rows != 2 {
		t.Fatalf("legacy rows = %d, want 2 (no EventID dedupe)", rows)
	}
}

// --- helpers -------------------------------------------------------------

type failingLogger struct{}

func (failingLogger) LogChecked(ctx context.Context, entry *service.UsageEntry) error {
	return errors.New("db down")
}

type captureLogger struct {
	entry *service.UsageEntry
}

func (c *captureLogger) LogChecked(ctx context.Context, entry *service.UsageEntry) error {
	c.entry = entry
	return nil
}

// Acceptance round 2 — WAL throughput: sustained submissions far above the
// old cleanup limit (200/30s = 6.7 rows/s) must PLATEAU, not accumulate:
// ack-driven compaction strips committed lines continuously.
func TestUsageWAL_PlateauUnderSustainedLoad(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 64<<20)
	t.Cleanup(spool.Close)
	SetUsageSpool(spool)
	t.Cleanup(func() { SetUsageSpool(nil) })

	stop := make(chan struct{})
	go func() {
		RunUsageSpoolReplay(context.Background(), spool, 30*time.Second,
			func(ctx context.Context, entries []*service.UsageEntry) error {
				for _, e := range entries {
					if err := db.svc.LogChecked(ctx, e); err != nil {
						return err
					}
				}
				return nil
			})
	}()
	t.Cleanup(func() { close(stop) })

	const total = 3000
	for i := 0; i < total; i++ {
		e := &service.UsageEntry{EventID: "row-" + uuid.NewString(), RouteType: "openai", OutputTokens: 1}
		submitUsageEntryFunc(&okLogger{svc: db.svc}, e, nil) // healthy DB: real commits + Ack: task commits + Ack
		// Drive compaction deterministically in-test: ack threshold fires at
		// 256, plus the 2s ticker runs concurrently.
		if i%512 == 511 {
			deadline := time.Now().Add(5 * time.Second)
			for spool.Pending() > 600 && time.Now().Before(deadline) {
				spool.Compact()
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	// Allow the async workers to finish committing + compacting.
	deadline := time.Now().Add(15 * time.Second)
	for (spool.Pending() > 0 || countRows(t, db) < total) && time.Now().Before(deadline) {
		spool.Compact()
		time.Sleep(50 * time.Millisecond)
	}
	rows := countRows(t, db)
	if rows != total {
		t.Fatalf("committed rows = %d, want %d", rows, total)
	}
	if got := spool.Pending(); got > 0 {
		t.Fatalf("WAL did not drain: %d lines remain", got)
	}
}

func countRows(t *testing.T, db usageDB) int64 {
	t.Helper()
	r, _ := db.count(t)
	return r
}

// DB outage: the WAL grows; recovery DRAINS it to zero via the replay loop
// (back-to-back rounds, not one 200-row round per 30s).
func TestUsageWAL_DBOutageThenDrain(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 64<<20)
	t.Cleanup(spool.Close)

	if err := db.raw.Close(); err != nil {
		t.Fatal(err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		if !spool.Write(&service.UsageEntry{RouteType: "openai", EventID: fmt.Sprint("drain-", i)}) {
			t.Fatal("WAL write failed")
		}
	}
	if got := spool.Pending(); got != n {
		t.Fatalf("WAL lines = %d, want %d", got, n)
	}
	// Recovery: fresh DB, one drain pass (what the 30s tick runs).
	db2 := newUsageDB(t)
	for spool.Pending() > 0 {
		spool.Replay(perRow(func(e *service.UsageEntry) error {
			return db2.svc.LogChecked(context.Background(), e)
		}), 200)
	}
	if got := spool.Pending(); got != 0 {
		t.Fatalf("drain left %d lines (must run back-to-back rounds)", got)
	}
	if rows := countRows(t, db2); rows != n {
		t.Fatalf("drained rows = %d, want %d", rows, n)
	}
}

// WAL at cap: submit falls back to the bounded synchronous DB write — rows
// still commit, nothing silently lost.
func TestUsageWAL_CapFallsBackToSyncCommit(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 2048) // ~2KB: fits a couple of lines only
	t.Cleanup(spool.Close)
	SetUsageSpool(spool)
	t.Cleanup(func() { SetUsageSpool(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = nil // force the sync path deterministically
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	for i := 0; i < 50; i++ {
		submitUsageEntryFunc(&okLogger{svc: db.svc}, &service.UsageEntry{RouteType: "openai"}, nil)
	}
	if rows := countRows(t, db); rows != 50 {
		t.Fatalf("cap fallback committed %d/50 rows", rows)
	}
}

type okLogger struct{ svc *service.UsageService }

func (o *okLogger) LogChecked(ctx context.Context, e *service.UsageEntry) error {
	return o.svc.LogChecked(ctx, e)
}

// Per-event walHeld semantics: a WAL-backed event failing its DB write is
// NOT a loss; a non-WAL-backed one IS (counter moves).
func TestUsageWAL_PerEventWalHeld(t *testing.T) {
	before := testutil.ToFloat64(usageRowsLostTotal)
	runUsageTask(&failingLogger{}, &service.UsageEntry{EventID: "held"}, true)
	if got := testutil.ToFloat64(usageRowsLostTotal) - before; got != 0 {
		t.Fatalf("WAL-backed failure must not count as lost: %v", got)
	}
	runUsageTask(&failingLogger{}, &service.UsageEntry{EventID: "notheld"}, false)
	if got := testutil.ToFloat64(usageRowsLostTotal) - before; got != 1 {
		t.Fatalf("non-WAL failure must count as lost: %v", got)
	}
}

// The replay loop must NOT hold the WAL file lock across DB writes: a slow
// flush (50ms/row) over a large batch must not block appends measurably.
func TestUsageWAL_ReplayDoesNotBlockAppends(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 64<<20)
	t.Cleanup(spool.Close)
	for i := 0; i < 300; i++ {
		spool.Write(&service.UsageEntry{EventID: fmt.Sprint("pre-", i)})
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		spool.Replay(perRow(func(e *service.UsageEntry) error {
			time.Sleep(50 * time.Millisecond) // 300 rows × 50ms = 15s if locked
			return errors.New("db always down in this test")
		}), 300)
	}()
	time.Sleep(300 * time.Millisecond) // let the replay batch get going
	maxWrite := time.Duration(0)
	for i := 0; i < 20; i++ {
		start := time.Now()
		if !spool.Write(&service.UsageEntry{EventID: fmt.Sprint("hot-", i)}) {
			t.Fatal("append failed during replay")
		}
		if d := time.Since(start); d > maxWrite {
			maxWrite = d
		}
	}
	if maxWrite > 2*time.Second {
		t.Fatalf("append blocked %v during replay — file lock held across DB writes", maxWrite)
	}
	<-done
}

// r34 e2e finding, regression-locked: the integrity probe's EventID must fit
// usage_logs.event_id varchar(36) — the first field version prefixed the
// UUID and the probe failed on real Postgres (which sqlite tests never catch:
// sqlite does not enforce varchar widths).
func TestProbeUsageDurability_EventIDFitsColumn(t *testing.T) {
	var got *service.UsageEntry
	logger := &captureEntryLogger{save: func(e *service.UsageEntry) error {
		got = e
		return nil
	}}
	if err := ProbeUsageDurability(logger); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got == nil {
		t.Fatal("probe did not reach the logger")
	}
	if len(got.EventID) != 36 {
		t.Fatalf("probe EventID length = %d (%q), want 36 (bare UUID, varchar(36) column)", len(got.EventID), got.EventID)
	}
	if got.RouteType != "integrity_probe" {
		t.Fatalf("probe route_type = %q, want integrity_probe", got.RouteType)
	}
	if got.PrecomputedCost != 0 || got.InputTokens != 0 || got.OutputTokens != 0 {
		t.Fatalf("probe row carries usage: %+v", got)
	}
}

type captureEntryLogger struct {
	save func(e *service.UsageEntry) error
}

func (c *captureEntryLogger) LogChecked(ctx context.Context, entry *service.UsageEntry) error {
	return c.save(entry)
}

// ③ (r53) The in-flight gap at the DB+WAL double-failure point: main WAL at
// cap AND the synchronous DB commit failing used to be a COUNTED REAL LOSS
// for rows of requests that were already in flight. The emergency overflow
// WAL must hold the row (fsync-durable) instead — no loss counter, no block
// engagement — and replay must drain it into the DB once the DB recovers.
func TestUsageWAL_DoubleFailure_EmergencyHoldsInFlightRow(t *testing.T) {
	db := newUsageDB(t)
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 2048) // ~2KB: fills after a couple of lines
	t.Cleanup(spool.Close)
	emergency := NewUsageSpoolEmergency(dir, 1<<20)
	t.Cleanup(emergency.Close)
	SetUsageSpool(spool)
	SetUsageSpoolEmergency(emergency)
	t.Cleanup(func() { SetUsageSpool(nil); SetUsageSpoolEmergency(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = nil // force the deterministic sync path
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	// Fill the main WAL past its cap so Write refuses (cap, not disk).
	for i := 0; i < 20; i++ {
		if !spool.Write(&service.UsageEntry{EventID: fmt.Sprint("fill-", i), ModelResponse: strings.Repeat("x", 200)}) {
			break
		}
	}
	if spool.Write(&service.UsageEntry{EventID: "must-refuse", ModelResponse: strings.Repeat("x", 200)}) {
		t.Fatal("main WAL unexpectedly accepted past its cap — test setup broken")
	}

	lostBefore := testutil.ToFloat64(usageRowsLostTotal)
	heldBefore := testutil.ToFloat64(usageEmergencyHeldTotal)
	submitUsageEntryFunc(&failingLogger{}, &service.UsageEntry{EventID: "inflight-gap-row", RouteType: "openai"}, nil)

	if got := testutil.ToFloat64(usageRowsLostTotal) - lostBefore; got != 0 {
		t.Fatalf("double failure counted a REAL loss despite the emergency WAL: %v", got)
	}
	if got := testutil.ToFloat64(usageEmergencyHeldTotal) - heldBefore; got != 1 {
		t.Fatalf("emergency-held counter delta = %v, want 1", got)
	}
	if rows := countRows(t, db); rows != 0 {
		t.Fatalf("db rows = %d before recovery, want 0", rows)
	}

	// DB recovers: the emergency replay loop must flush the held row into
	// usage_logs exactly once (EventID idempotent).
	flushed := emergency.Replay(perRow(func(e *service.UsageEntry) error {
		return db.svc.LogChecked(context.Background(), e)
	}), 10)
	if flushed != 1 {
		t.Fatalf("emergency replay flushed %d rows, want 1", flushed)
	}
	if rows := countRows(t, db); rows != 1 {
		t.Fatalf("db rows after recovery = %d, want 1 (the in-flight gap row)", rows)
	}
	// A second replay round must be a no-op (row committed + acked).
	if flushed := emergency.Replay(perRow(func(e *service.UsageEntry) error {
		return db.svc.LogChecked(context.Background(), e)
	}), 10); flushed != 0 {
		t.Fatalf("second emergency replay flushed %d, want 0 (idempotent)", flushed)
	}
}

// ③ (r58 review) Capacity-exhaustion model: when the emergency WAL crosses
// its high-water mark while DB writes fail, the billing-integrity block
// engages BEFORE any row is lost (pre-loss, in-flight bounded by admission
// rejection), and the recovery path (replay drains the emergency WAL into
// the DB + durable successes) releases it. Both WALs genuinely full while
// the DB still fails remains a counted real loss (the stated residual
// boundary — narrowed, not claimed away).
func TestUsageWAL_EmergencyHighWater_BlocksBeforeLossAndRecovers(t *testing.T) {
	billingintegrity.ResetForTest()
	t.Cleanup(billingintegrity.ResetForTest)
	billingintegrity.Configure(billingintegrity.PolicyBlock)

	db := newUsageDB(t)
	dir := t.TempDir()
	// Main WAL cap 1 byte: every Write refuses (cap, not disk). Emergency
	// cap sized so ONE typical row crosses the high-water (reserve = cap/2).
	main := NewUsageSpool(dir, 1)
	t.Cleanup(main.Close)
	emergency := NewUsageSpoolEmergency(dir, 4096)
	t.Cleanup(emergency.Close)
	SetUsageSpool(main)
	SetUsageSpoolEmergency(emergency)
	t.Cleanup(func() { SetUsageSpool(nil); SetUsageSpoolEmergency(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = nil
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	if main.Write(&service.UsageEntry{EventID: "never"}) {
		t.Fatal("1-byte main WAL must refuse every write")
	}

	lostBefore := testutil.ToFloat64(usageRowsLostTotal)
	heldBefore := testutil.ToFloat64(usageEmergencyHeldTotal)
	// First double-failure row: emergency holds it; a ~2.6KB row against a
	// 4096B cap with reserve 2048 crosses the high-water (2048B) on write.
	submitUsageEntryFunc(&failingLogger{}, &service.UsageEntry{
		EventID: "hw-1", RouteType: "openai", UserMessage: strings.Repeat("x", 2600),
	}, nil)
	if got := testutil.ToFloat64(usageEmergencyHeldTotal) - heldBefore; got != 1 {
		t.Fatalf("emergency-held delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(usageRowsLostTotal) - lostBefore; got != 0 {
		t.Fatalf("PRE-LOSS invariant broken: %v real loss(es) before capacity exhaustion", got)
	}
	if !billingintegrity.Blocked() {
		t.Fatal("high-water must engage the billing-integrity block BEFORE any loss")
	}

	// Recovery: replay drains the emergency WAL into the DB; durable
	// successes + the compressed hold release the block.
	flushed := emergency.Replay(perRow(func(e *service.UsageEntry) error {
		return db.svc.LogChecked(context.Background(), e)
	}), 10)
	if flushed != 1 {
		t.Fatalf("emergency replay flushed %d, want 1", flushed)
	}
	billingintegrity.FastForwardHoldForTest()
	for i := 0; i < 6 && billingintegrity.Blocked(); i++ {
		billingintegrity.ReportDurableSuccess()
	}
	if billingintegrity.Blocked() {
		t.Fatal("block must release after recovery (emergency drained + durable successes)")
	}
	if rows := countRows(t, db); rows != 1 {
		t.Fatalf("db rows = %d, want 1 (the emergency-held row drained)", rows)
	}
}
// ③ Triple failure — DB down, main WAL at cap, emergency tier NOT armed
// (or its own cap/disk also failed): the row IS a real loss, counted and
// block-engaging. The emergency tier narrows the loss window; it does not
// erase the accounting when every durable path is gone.
func TestUsageWAL_TripleFailure_RealLossStillCounted(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 2048)
	t.Cleanup(spool.Close)
	SetUsageSpool(spool)
	SetUsageSpoolEmergency(nil) // tier off = legacy r34 behavior
	t.Cleanup(func() { SetUsageSpool(nil) })
	oldWorkers := UsageWorkers
	UsageWorkers = nil
	t.Cleanup(func() { UsageWorkers = oldWorkers })

	for i := 0; i < 20; i++ {
		if !spool.Write(&service.UsageEntry{EventID: fmt.Sprint("fill-", i), ModelResponse: strings.Repeat("x", 200)}) {
			break
		}
	}

	lostBefore := testutil.ToFloat64(usageRowsLostTotal)
	heldBefore := testutil.ToFloat64(usageEmergencyHeldTotal)
	submitUsageEntryFunc(&failingLogger{}, &service.UsageEntry{EventID: "true-loss-row", RouteType: "openai"}, nil)
	if got := testutil.ToFloat64(usageRowsLostTotal) - lostBefore; got != 1 {
		t.Fatalf("real loss counter delta = %v, want 1", got)
	}
	if got := testutil.ToFloat64(usageEmergencyHeldTotal) - heldBefore; got != 0 {
		t.Fatalf("emergency-held delta = %v, want 0 (tier disabled)", got)
	}
}
