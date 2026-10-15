package handler

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crosslink/internal/service"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Usage WAL: a local append-only JSONL write-ahead log for BILLING events.
// Protocol (acceptance round 2): every event is fsync-durable here BEFORE it
// becomes an in-memory queue item; a successful DB commit ACKS the event and
// a background compactor strips acked lines (ack-driven compaction — the WAL
// plateaus in steady state instead of growing until the 30s replay strips
// it). Replay exists for crash/DB-outage recovery only; it takes a bounded
// SNAPSHOT of lines under the file lock and performs DB writes OUTSIDE the
// lock (the first version held the mutex across up to 200×10s of DB writes —
// up to ~2000s of blocked WAL appends).
//
// Loss semantics: a row leaves the WAL ONLY when its DB commit is confirmed
// (direct Ack, or replay flush nil). While the DB is down the WAL accumulates
// up to its byte cap; beyond the cap callers fall back to a bounded
// synchronous DB write (worker.go) — a loss happens only if BOTH fail and is
// counted. The reverse crash window (committed, line not yet compacted away)
// replays as a no-op via the EventID unique index — no double billing. Every
// append is fsynced; corrupt/torn lines are dropped LOUDLY.
//
// Multi-replica: per-instance volume (StatefulSet PVC); replay is idempotent,
// so recovering an instance's WAL on any replica is safe.

var (
	usageSpoolBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cl_usage_spool_bytes",
		Help: "Current size of the on-disk usage WAL.",
	})
	usageSpoolEvents = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "cl_usage_spool_events",
		Help: "Lines currently in the usage WAL (acked lines are removed by the compactor; must plateau under sustained load).",
	})
	usageSpoolReplayedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_usage_spool_replayed_total",
		Help: "Usage rows recovered from the WAL and acked by the database.",
	})
	usageSpoolCompactedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_usage_spool_compacted_total",
		Help: "Lines removed from the usage WAL by ack-driven compaction.",
	})
	usageSpoolDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_usage_spool_dropped_total",
		Help: "Usage rows dropped because the WAL hit its size cap (callers then use the synchronous DB fallback).",
	})
	usageSpoolCorruptTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_usage_spool_corrupt_total",
		Help: "WAL lines dropped as torn/invalid JSON. Counted, never silent.",
	})
)

// ackCompactThreshold: compaction runs when this many distinct events are
// pending removal (or on the 2s ticker). Small enough that the WAL stays
// tight at high rates, large enough to amortize rewrites.
const ackCompactThreshold = 256

type UsageSpool struct {
	mu       sync.Mutex // protects the WAL file (append/rewrite) — held for FILE I/O only, never DB writes
	path     string
	maxBytes int64

	pending atomic.Int64 // lines currently in the WAL (exact on write/compact)

	ackMu sync.Mutex
	acked map[string]struct{}

	lastCompactAt time.Time // guarded by ackMu — amortizes rewrites (CompactIfWorthwhile)

	compactSoon chan struct{} // buffered-1 trigger
}

// NewUsageSpool prepares the WAL directory. dir=="" disables spooling.
func NewUsageSpool(dir string, maxBytes int64) *UsageSpool {
	if dir == "" {
		return nil
	}
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("usage spool: cannot create directory, spooling disabled", "dir", dir, "error", err)
		return nil
	}
	s := &UsageSpool{
		path:        filepath.Join(dir, "usage-spill.jsonl"),
		maxBytes:    maxBytes,
		acked:       make(map[string]struct{}),
		compactSoon: make(chan struct{}, 1),
	}
	if err := s.sanitizeTail(); err != nil {
		slog.Error("usage spool: tail sanitize failed", "error", err)
	}
	if st, err := os.Stat(s.path); err == nil {
		usageSpoolBytes.Set(float64(st.Size()))
		s.refreshEventCount()
		slog.Warn("usage spool: recovering leftover WAL from a previous run",
			"path", s.path, "bytes", st.Size())
	}
	return s
}

// sanitizeTail drops a torn final line (crash mid-append). Rows are only
// ever appended after a sanitize, so a non-newline-terminated tail is by
// definition torn.
func (s *UsageSpool) sanitizeTail() error {
	f, err := os.OpenFile(s.path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	buf := make([]byte, 1)
	if _, err := f.ReadAt(buf, st.Size()-1); err != nil {
		return err
	}
	if buf[0] == '\n' {
		return nil
	}
	size := st.Size()
	chunk := make([]byte, 4096)
	for size > 0 {
		read := int64(len(chunk))
		if read > size {
			read = size
		}
		start := size - read
		if _, err := f.ReadAt(chunk[:read], start); err != nil {
			return err
		}
		for i := read - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				return f.Truncate(start + i + 1)
			}
		}
		size = start
	}
	return f.Truncate(0)
}

// Write persists one event (fsync) BEFORE the caller hands it to any
// in-memory structure. Returns false when not persisted (WAL disabled, at
// cap, or I/O error) — callers must then treat the event as NOT durable.
func (s *UsageSpool) Write(entry *service.UsageEntry) bool {
	if s == nil {
		return false
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("usage spool: open failed", "error", err)
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		slog.Error("usage spool: stat failed", "error", err)
		return false
	}
	if st.Size()+int64(len(data))+1 > s.maxBytes {
		usageSpoolDroppedTotal.Inc()
		return false
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		slog.Error("usage spool: write failed", "error", err)
		return false
	}
	if err := f.Sync(); err != nil {
		slog.Error("usage spool: fsync failed", "error", err)
		return false
	}
	usageSpoolBytes.Set(float64(st.Size() + int64(len(data)) + 1))
	usageSpoolEvents.Inc()
	s.pending.Add(1)
	return true
}

// Ack marks an event durably committed: the compactor will strip its WAL
// line. Cheap (in-memory set + occasional trigger); safe to call for events
// whose line was already compacted or replayed.
func (s *UsageSpool) Ack(eventID string) {
	if s == nil || eventID == "" {
		return
	}
	s.ackMu.Lock()
	s.acked[eventID] = struct{}{}
	n := len(s.acked)
	s.ackMu.Unlock()
	if n >= ackCompactThreshold {
		select {
		case s.compactSoon <- struct{}{}:
		default:
		}
	}
}

// walRename/walSync are OS seams (vars ONLY so fault-injection tests can
// force failures; production never reassigns — same pattern as
// guardrail.lookupIPAddr). A failed rename/sync must leave the ORIGINAL WAL
// intact and the ack snapshot unconsumed.
var (
	walRename = os.Rename
	walSync   = func(f *os.File) error { return f.Sync() }
)

// walLineKey returns the compaction key for one WAL line: the EventID for
// current lines, or a deterministic synthetic key for LEGACY lines written
// before EventIDs existed (they are legal billing rows — replay must flush
// and ack them by the same synthetic key, never silently drop them).
func walLineKey(payload []byte) (string, bool) {
	var probe struct {
		EventID string `json:"EventID"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return "", false // corrupt
	}
	if probe.EventID != "" {
		return probe.EventID, true
	}
	sum := sha256.Sum256(payload)
	return "legacy:" + hex.EncodeToString(sum[:8]), true
}

// quarantine preserves a corrupt/torn raw line in a sidecar file before it
// is dropped from the WAL: forensics + counted alert, never a silent loss
// and never a false "already counted elsewhere".
func (s *UsageSpool) quarantine(line []byte) {
	usageSpoolCorruptTotal.Inc()
	f, err := os.OpenFile(s.path+".corrupt", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		slog.Error("usage spool: quarantine open failed (line LOST from WAL, kept in metrics only)", "error", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		slog.Error("usage spool: quarantine write failed", "error", err)
	}
}

// rewriteStream is the pure-I/O core of Compact: it copies every line whose
// key is NOT in ackSet to dst. Return contract (supervisor fault review):
// on ANY failure (non-EOF read error, write error, flush error) it returns
// the error and the caller must keep the original file, delete the temp and
// NOT consume the ack snapshot. A torn tail (data without '\n' + io.EOF) is
// quarantined and dropped; a legacy no-EventID line is KEPT under its
// synthetic key.
func (s *UsageSpool) rewriteStream(src io.Reader, w *bufio.Writer, ackSet map[string]struct{}) (removed, kept int, err error) {
	br := bufio.NewReaderSize(src, 64<<10)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) == 0 {
			if rerr != nil && rerr != io.EOF {
				return removed, kept, fmt.Errorf("wal read: %w", rerr)
			}
			break // clean EOF
		}
		if rerr != nil && rerr != io.EOF {
			// Mid-file I/O failure: ABORT — the file must not be replaced by
			// a truncated copy.
			return removed, kept, fmt.Errorf("wal read: %w", rerr)
		}
		if rerr == io.EOF {
			// Data without the trailing newline = torn append tail.
			s.quarantine(line)
			break
		}
		payload := line[:len(line)-1]
		key, ok := walLineKey(payload)
		if !ok {
			// Corrupt line: quarantine (sidecar + counted), keep scanning.
			s.quarantine(line)
			continue
		}
		if _, done := ackSet[key]; done {
			removed++
			continue
		}
		if _, werr := w.Write(line); werr != nil {
			return removed, kept, fmt.Errorf("wal write: %w", werr)
		}
		kept++
	}
	if ferr := w.Flush(); ferr != nil {
		return removed, kept, fmt.Errorf("wal flush: %w", ferr)
	}
	return removed, kept, nil
}

// Compact rewrites the WAL without acked lines. Failure protocol (supervisor
// fault review): ANY failure — read I/O, write, flush, fsync, rename — keeps
// the ORIGINAL file byte-identical, removes the temp file, and leaves the
// ack snapshot in the live set (acks are consumed ONLY after the atomic
// swap succeeds; otherwise throughput would silently regress to replay-only
// stripping). File I/O only (no DB calls) under mu; crash-safe via
// tmp+fsync+rename with the Windows open-handle trap handled.
func (s *UsageSpool) Compact() int {
	if s == nil {
		return 0
	}
	s.ackMu.Lock()
	if len(s.acked) == 0 {
		s.ackMu.Unlock()
		return 0
	}
	ackSet := make(map[string]struct{}, len(s.acked))
	for id := range s.acked {
		ackSet[id] = struct{}{}
	}
	s.ackMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No WAL at all: nothing to strip; consume the snapshot (there is
			// no file left to keep lines in).
			s.consumeAcks(ackSet)
			return len(ackSet)
		}
		slog.Error("usage spool: compact open failed (acks retained)", "error", err)
		return 0
	}
	tmp := s.path + ".tmp"
	fail := func() {
		src.Close()
		os.Remove(tmp) // best-effort; a leftover .tmp never replaces the WAL
	}
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		fail()
		slog.Error("usage spool: compact temp open failed (acks retained)", "error", err)
		return 0
	}
	w := bufio.NewWriter(dst)
	removed, kept, rerr := s.rewriteStream(src, w, ackSet)
	if rerr != nil {
		dst.Close()
		fail()
		slog.Error("usage spool: compact aborted, ORIGINAL WAL KEPT, acks retained", "error", rerr)
		return 0
	}
	src.Close() // Windows: rename over an open handle fails
	if serr := walSync(dst); serr != nil {
		dst.Close()
		fail()
		slog.Error("usage spool: compact fsync failed, ORIGINAL WAL KEPT, acks retained", "error", serr)
		return 0
	}
	dst.Close()
	if rerr := walRename(tmp, s.path); rerr != nil {
		os.Remove(tmp)
		slog.Error("usage spool: compact rename failed, ORIGINAL WAL KEPT, acks retained", "error", rerr)
		return 0
	}
	// Atomic swap succeeded — NOW the snapshot's acks are durably reflected.
	s.consumeAcks(ackSet)
	usageSpoolCompactedTotal.Add(float64(removed))
	if st, err := os.Stat(s.path); err == nil {
		usageSpoolBytes.Set(float64(st.Size()))
	}
	usageSpoolEvents.Set(float64(kept))
	s.pending.Store(int64(kept))
	slog.Debug("usage spool: compacted", "removed", removed, "kept", kept)
	return removed
}

// consumeAcks removes a consumed snapshot from the live ack set ONLY (acks
// arriving during the file pass keep their entries).
func (s *UsageSpool) consumeAcks(snapshot map[string]struct{}) {
	s.ackMu.Lock()
	for id := range snapshot {
		delete(s.acked, id)
	}
	s.ackMu.Unlock()
}

// CompactIfWorthwhile amortizes the O(file) rewrite that Compact performs.
// A full rewrite is only worth its cost when it removes a substantial part of
// the file (>= half its lines), when the file is small (rewrites cheap), or
// when the staleness bound (30s since the last compaction) fires so acked
// lines cannot pile up on disk indefinitely. Without this, recovering a large
// backlog pays one full rewrite per 200-row replay round plus one per 2s
// ticker fire — the disk saturates on rewrites and the drain collapses to
// ~1 round per tick (observed: 158k-line backlog draining at 6.7 rows/s).
// Deferred acks stay in memory; a crash before compaction replays them
// idempotently (EventID unique index), so deferral never risks double billing.
func (s *UsageSpool) CompactIfWorthwhile() {
	if s == nil {
		return
	}
	s.ackMu.Lock()
	n := len(s.acked)
	stale := time.Since(s.lastCompactAt) > 30*time.Second
	s.ackMu.Unlock()
	if n == 0 {
		return
	}
	total := s.Pending()
	if total <= 4096 || int64(n) >= total/2 || stale {
		s.ackMu.Lock()
		s.lastCompactAt = time.Now()
		s.ackMu.Unlock()
		s.Compact()
	}
}

func (s *UsageSpool) refreshEventCount() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		return
	}
	defer f.Close()
	n := 0
	br := bufio.NewReader(f)
	for {
		line, rerr := br.ReadBytes('\n')
		if len(line) == 0 {
			break
		}
		if rerr == nil {
			n++
		}
	}
	usageSpoolEvents.Set(float64(n))
	s.pending.Store(int64(n))
}

// pendingApprox reports the exact in-memory line counter — the drain-loop
// progress signal.
func (s *UsageSpool) Pending() int64 {
	if s == nil {
		return 0
	}
	return s.pending.Load()
}

// Replay flushes WAL lines through flush (nil return = durably stored) and
// returns how many rows the DB accepted this round.
// LOCK PROTOCOL: it reads a bounded snapshot of lines under mu (file read
// only), then performs ALL DB writes with mu RELEASED, then ACKS the
// committed ids so the compactor strips them. Unacked lines (flush failure)
// stay on disk untouched for the next round; the round stops at the first
// failure so a slow/dead DB cannot stall writes. max bounds the snapshot so
// memory stays one round's worth of lines.
//
// The return value (not Pending()) is the drain loop's progress signal:
// Pending() counts ALL lines including concurrent new arrivals, so under
// live traffic a round that flushes 200 while 300 new lines land DECREASES
// nothing — a Pending()-based progress check would break the drain after
// one round per tick and a large backlog would take hours to recover.
func (s *UsageSpool) Replay(flush func(entry *service.UsageEntry) error, max int) int {
	if s == nil {
		return 0
	}
	if max <= 0 {
		max = 200
	}
	type item struct {
		id    string
		entry *service.UsageEntry
	}
	batch := make([]item, 0, max)
	_ = 0 // (corrupt accounting moved to quarantine)

	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		f, err := os.Open(s.path)
		if err != nil {
			return
		}
		defer f.Close()
		br := bufio.NewReaderSize(f, 64<<10)
		for len(batch) < max {
			line, rerr := br.ReadBytes('\n')
			if len(line) == 0 {
				if rerr != nil && rerr != io.EOF {
					slog.Warn("usage spool: snapshot read failed mid-file, round truncated (lines stay on disk)", "error", rerr)
				}
				break
			}
			if rerr != nil && rerr != io.EOF {
				slog.Warn("usage spool: snapshot read failed mid-file, round truncated (lines stay on disk)", "error", rerr)
				break
			}
			if rerr == io.EOF {
				// Torn tail: leave for Compact (the single quarantine/count
				// point) — never flush a partial row.
				break
			}
			payload := line[:len(line)-1]
			var entry service.UsageEntry
			if uerr := json.Unmarshal(payload, &entry); uerr != nil {
				// Corrupt line: skip silently here; Compact quarantines AND
				// counts it exactly once at removal (single counting point —
				// no double-report between snapshot and rewrite).
				continue
			}
			// Legacy pre-EventID lines are LEGAL billing rows: flush them and
			// ack under the deterministic synthetic key so Compact can strip
			// them (LogChecked mints a one-shot UUID for the DB row).
			id := entry.EventID
			if id == "" {
				sum := sha256.Sum256(payload)
				id = "legacy:" + hex.EncodeToString(sum[:8])
			}
			batch = append(batch, item{id: id, entry: &entry})
		}
	}()
	if len(batch) == 0 {
		return 0
	}

	flushed := 0
	for _, it := range batch {
		if err := flush(it.entry); err != nil {
			// DB ack failed: keep this row AND every later row on disk.
			break
		}
		s.Ack(it.id)
		flushed++
	}
	usageSpoolReplayedTotal.Add(float64(flushed))
	if flushed > 0 {
		// Strip what just committed (plus anything acked meanwhile) — through
		// the amortized gate: a big backlog must not pay one full-file rewrite
		// per round (see CompactIfWorthwhile).
		s.CompactIfWorthwhile()
	}
	if stopped := flushed < len(batch); stopped {
		slog.Warn("usage spool: replay round stopped early (db ack failed)",
			"flushed", flushed, "pending", len(batch)-flushed)
	}
	return flushed
}

// RunUsageSpoolReplay drives recovery replay and ack-driven compaction:
//   - replay tick (default 30s): drains the WAL in back-to-back rounds while
//     progress is made (crash/DB-outage recovery; steady state relies on Ack
//   - compaction instead);
//   - compaction tick (2s) + threshold trigger: strips acked lines so the
//     WAL plateaus under sustained load.
func RunUsageSpoolReplay(ctx context.Context, spool *UsageSpool, interval time.Duration, flush func(ctx context.Context, entry *service.UsageEntry) error) {
	if spool == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	replayT := time.NewTicker(interval)
	defer replayT.Stop()
	compactT := time.NewTicker(2 * time.Second)
	defer compactT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-spool.compactSoon:
			spool.CompactIfWorthwhile()
		case <-compactT.C:
			spool.CompactIfWorthwhile()
		case <-replayT.C:
			// Drain mode: keep rounds going while the DB accepts rows, so a
			// recovered DB clears a large backlog without waiting a full
			// interval per 200 rows. Progress is measured by the round's own
			// flushed count — NOT Pending(), which counts concurrent new
			// arrivals too and would falsely report "no progress" under live
			// traffic (a 160k-line backlog then drains at one round per tick).
			for round := 0; round < 200; round++ {
				flushed := spool.Replay(func(entry *service.UsageEntry) error {
					fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					return flush(fctx, entry)
				}, 200)
				if flushed == 0 || spool.Pending() == 0 {
					break // DB refusing rows (or nothing left but this round's acks)
				}
			}
		}
	}
}
