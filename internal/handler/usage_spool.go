package handler

import (
	"bufio"
	"context"
	"errors"
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

	"github.com/crosslink/internal/billingintegrity"
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

	// replayCursor is the byte offset of the next UNflushed WAL line (guarded
	// by mu). It lets drain rounds skip acked-but-uncompacted lines instead of
	// re-flushing them; see Replay. Reset to 0 by Compact (the rewrite moves
	// every line) — a stale cursor only causes idempotent re-reads.
	replayCursor int64

	compactSoon chan struct{} // buffered-1 trigger

	// GROUP COMMIT: concurrent Writers append to one persistent handle and a
	// single syncer goroutine fsyncs ONCE per batch. Each Write still waits
	// for an fsync THAT INCLUDES ITS OWN BYTES before returning true — the
	// per-event WAL-before-enqueue durability contract is preserved; only the
	// fsync COUNT collapses (one per batch instead of one per event). On slow
	// disks (measured: 0.5-1s per FlushFileBuffers, ~35 events/s = >100% of
	// one disk) per-event fsync starves everything sharing the volume.
	appF    *os.File // persistent append handle (guarded by mu)
	appSeq  int64    // appended-but-not-yet-synced event counter (guarded by mu)
	syncMu    sync.Mutex
	syncedSeq int64      // appSeq covered by a COMPLETED fsync (syncMu)
	syncWake  chan struct{}
	closed    bool       // Close() called (syncMu): syncer exits, waiters released
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
		syncWake:    make(chan struct{}, 1),
	}
	if err := s.sanitizeTail(); err != nil {
		slog.Error("usage spool: tail sanitize failed", "error", err)
	}
	if err := s.openAppend(); err != nil {
		slog.Error("usage spool: cannot open append handle, spooling disabled", "error", err)
		return nil
	}
	go s.syncLoop()
	if st, err := os.Stat(s.path); err == nil {
		usageSpoolBytes.Set(float64(st.Size()))
		s.refreshEventCount()
		slog.Warn("usage spool: recovering leftover WAL from a previous run",
			"path", s.path, "bytes", st.Size())
	}
	return s
}

// openAppend (re)creates the persistent append handle. Callers hold mu or
// single-threaded startup.
func (s *UsageSpool) openAppend() error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.appF = f
	return nil
}
// Close releases the persistent append handle and stops the group-commit
// syncer; waiters are released (the spool must not be used afterwards).
// Tests need it so t.TempDir cleanup can delete the WAL on Windows, where an
// open handle locks the file; the production spool lives for the process
// lifetime and is never closed.
func (s *UsageSpool) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.appF != nil {
		s.appF.Close()
		s.appF = nil
	}
	s.mu.Unlock()
	s.syncMu.Lock()
	s.closed = true
	s.syncedSeq = s.appSeq
	s.syncMu.Unlock()
}

// syncLoop is the group-commit flusher. DURABILITY WATERMARK RULE: the
// confirmed sequence number is captured BEFORE the fsync starts, under the
// same mu that appends take — an fsync only ever confirms appends that were
// already in the file when the syscall began. Reading appSeq AFTER the fsync
// (the original bug, caught in review) let a concurrent Write that appended
// mid-fsync be falsely confirmed: its bytes were never covered by a completed
// fsync, so a crash could lose a billing event whose Write had returned true.
// Appends after the watermark simply wait for the NEXT successful fsync (the
// 50ms fallback tick guarantees progress without new wakes).
func (s *UsageSpool) syncLoop() {
	for {
		s.syncMu.Lock()
		closed := s.closed
		s.syncMu.Unlock()
		if closed {
			return
		}
		select {
		case <-s.syncWake:
		case <-time.After(50 * time.Millisecond):
		}
		// Capture handle AND watermark atomically with respect to appends
		// (and to Compact's handle swap, which also holds mu): everything up
		// to target is what this fsync may confirm.
		s.mu.Lock()
		f := s.appF
		target := s.appSeq
		s.mu.Unlock()
		if f == nil {
			continue
		}
		if err := walSync(f); err != nil {
			// Handle closed/swapped by a concurrent Compact: its rewrite
			// fsynced the kept bytes before the atomic rename, and Compact
			// advances syncedSeq itself under the same watermark rule.
			if errors.Is(err, os.ErrClosed) {
				continue
			}
			slog.Warn("usage spool: group fsync failed (retrying)", "error", err)
			continue
		}
		s.confirmSynced(target)
	}
}

// confirmSynced advances the durability watermark monotonically. Never
// regresses (a late fsync covering an older target must not undo a newer
// confirmation, e.g. one advanced by Compact's atomic replace).
func (s *UsageSpool) confirmSynced(target int64) {
	s.syncMu.Lock()
	if target > s.syncedSeq {
		s.syncedSeq = target
	}
	s.syncMu.Unlock()
}

// waitSynced blocks until an fsync covering event seq has completed, or the
// timeout expires (disk stalled): false means the event is NOT confirmed
// durable and callers must treat Write as failed (synchronous DB fallback).
// Polling instead of sync.Cond: a Cond broadcast that lands outside a Wait
// window is lost, and with a stalled disk there is no later broadcast to
// recover with — the writer would hang past its deadline.
func (s *UsageSpool) waitSynced(seq int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		s.syncMu.Lock()
		done := s.syncedSeq >= seq
		s.syncMu.Unlock()
		if done {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
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

// Write persists one event durably BEFORE the caller hands it to any
// in-memory structure: the bytes are appended and the writer then WAITS for
// a completed fsync that includes them (group commit — the syncer batches
// concurrent writers into one fsync; per-event durability is unchanged, only
// the fsync count collapses). Returns false when durability was not confirmed
// within the bounded wait (WAL disabled, at cap, I/O error, stalled disk) —
// callers must then treat the event as NOT durable.
func (s *UsageSpool) Write(entry *service.UsageEntry) bool {
	if s == nil {
		return false
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return false
	}
	s.mu.Lock()
	if s.appF == nil {
		s.mu.Unlock()
		return false
	}
	st, err := s.appF.Stat()
	if err != nil {
		s.mu.Unlock()
		slog.Error("usage spool: stat failed", "error", err)
		return false
	}
	if st.Size()+int64(len(data))+1 > s.maxBytes {
		s.mu.Unlock()
		usageSpoolDroppedTotal.Inc()
		return false
	}
	if _, err := s.appF.Write(append(data, '\n')); err != nil {
		s.mu.Unlock()
		slog.Error("usage spool: write failed", "error", err)
		return false
	}
	s.appSeq++
	seq := s.appSeq
	s.mu.Unlock()

	// Durability barrier: wake the group flusher and wait for OUR bytes.
	select {
	case s.syncWake <- struct{}{}:
	default:
	}
	if !s.waitSynced(seq, 5*time.Second) {
		slog.Error("usage spool: fsync not confirmed in time — event treated as not durable",
			"event_id", entry.EventID)
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
	// The persistent append handle must be closed for the atomic rename
	// (Windows) and is reopened on every exit path: on failure the original
	// file (with its possibly-unsynced tail) is still the live WAL; on
	// success appends continue on the rewritten file.
	if s.appF != nil {
		s.appF.Close()
		s.appF = nil
	}
	reopenAppend := func() {
		if err := s.openAppend(); err != nil {
			slog.Error("usage spool: append handle reopen failed after compact", "error", err)
		}
	}
	src, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			// No WAL at all: nothing to strip; consume the snapshot (there is
			// no file left to keep lines in).
			s.consumeAcks(ackSet)
			reopenAppend()
			return len(ackSet)
		}
		slog.Error("usage spool: compact open failed (acks retained)", "error", err)
		reopenAppend()
		return 0
	}
	tmp := s.path + ".tmp"
	fail := func() {
		src.Close()
		os.Remove(tmp) // best-effort; a leftover .tmp never replaces the WAL
		reopenAppend()
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
		reopenAppend()
		return 0
	}
	// Atomic swap succeeded — NOW the snapshot's acks are durably reflected.
	s.consumeAcks(ackSet)
	// The rewrite moved every surviving line: the drain cursor must restart
	// from the front (the first unflushed line is now at offset 0).
	s.replayCursor = 0
	reopenAppend()
	// Kept bytes are durable (the tmp was fsynced before the rename), so any
	// writer waiting on the group-commit barrier for pre-compact appends is
	// released: their lines survived the rewrite and are on stable storage.
	// Compact holds mu here, so this appSeq read is atomic with appends —
	// and it only ever ADVANCES the watermark (monotone rule).
	confirmTarget := s.appSeq
	usageSpoolCompactedTotal.Add(float64(removed))
	s.confirmSynced(confirmTarget)
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

// Size returns the current WAL file size in bytes (0 when absent).
func (s *UsageSpool) Size() int64 {
	if s == nil {
		return 0
	}
	if st, err := os.Stat(s.path); err == nil {
		return st.Size()
	}
	return 0
}

// Replay flushes one WAL round through flushBatch (nil = every row durably
// stored — conflict-noop rows count as present) and returns how many rows the
// DB accepted. The round is ONE batch call: per-row commits measured at
// ~70-140ms each under live load (one fsync round trip per transaction), which
// capped drain at ~10 rows/s; batching amortizes the commit across the round.
// LOCK PROTOCOL: it reads a bounded snapshot of lines under mu (file read
// only), then performs the DB write with mu RELEASED, then ACKS the committed
// ids so the compactor strips them. On flush failure the cursor parks at the
// batch's first line and every row stays on disk for the next round.
//
// The return value (not Pending()) is the drain loop's progress signal:
// Pending() counts ALL lines including concurrent new arrivals, so under
// live traffic a round that flushes 200 while 300 new lines land DECREASES
// nothing — a Pending()-based progress check would break the drain after
// one round per tick and a large backlog would take hours to recover.
//
// DRAIN CURSOR: each round resumes at the byte offset just past the last row
// the DB accepted (replayCursor), skipping acked-but-not-yet-compacted lines.
// Re-reading from offset 0 would re-flush the SAME acked lines every round
// whenever compaction is deferred (CompactIfWorthwhile) — the drain then only
// advances when a compaction finally strips them (observed: exactly one
// 200-row round per 30s staleness strip). The cursor resets to 0 whenever the
// file is rewritten (Compact) or the process restarts; a stale cursor only
// ever re-reads already-committed rows, which flush idempotently.
func (s *UsageSpool) Replay(flushBatch func(entries []*service.UsageEntry) error, max int) int {
	if s == nil {
		return 0
	}
	if max <= 0 {
		max = 200
	}
	type item struct {
		id    string
		entry *service.UsageEntry
		off   int64 // byte offset of the line's FIRST byte
	}
	batch := make([]item, 0, max)

	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		f, err := os.Open(s.path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(s.replayCursor, io.SeekStart); err != nil {
			slog.Warn("usage spool: replay seek failed, restarting from front", "error", err)
			s.replayCursor = 0
		}
		off := s.replayCursor
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
				// point) — never flush a partial row. Cursor stays at the
				// last complete line.
				break
			}
			start := off
			off += int64(len(line))
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
			// them (LogBatchChecked mints a one-shot UUID for the DB row).
			id := entry.EventID
			if id == "" {
				sum := sha256.Sum256(payload)
				id = "legacy:" + hex.EncodeToString(sum[:8])
			}
			// Already committed (acked, awaiting compaction): skip past it —
			// re-flushing is idempotent but pure waste.
			s.ackMu.Lock()
			_, done := s.acked[id]
			s.ackMu.Unlock()
			if done {
				continue
			}
			batch = append(batch, item{id: id, entry: &entry, off: start})
		}
		// The cursor may move past skipped (corrupt/acked) lines even when
		// nothing new flushes: those bytes never need re-reading.
		s.replayCursor = off
	}()
	if len(batch) == 0 {
		return 0
	}

	entries := make([]*service.UsageEntry, len(batch))
	ids := make([]string, len(batch))
	for i, it := range batch {
		entries[i] = it.entry
		ids[i] = it.id
	}
	flushed := 0
	if err := flushBatch(entries); err != nil {
		// DB refused the batch: park the cursor at its first line so the next
		// round retries exactly these rows.
		s.mu.Lock()
		s.replayCursor = batch[0].off
		s.mu.Unlock()
	} else {
		for _, id := range ids {
			s.Ack(id)
		}
		flushed = len(batch)
	}
	usageSpoolReplayedTotal.Add(float64(flushed))
	if flushed > 0 {
		// Replay commits prove DB durability end-to-end — count them toward
		// integrity-block recovery (r34: with the gate rejecting new traffic
		// and an empty backlog there is no other organic success source).
		for i := 0; i < flushed; i++ {
			billingintegrity.ReportDurableSuccess()
		}
		// Strip what just committed (plus anything acked meanwhile) — through
		// the amortized gate: a big backlog must not pay one full-file rewrite
		// per round (see CompactIfWorthwhile).
		s.CompactIfWorthwhile()
	}
	if flushed == 0 {
		slog.Warn("usage spool: replay round failed (db refused batch)",
			"rows", len(batch))
	}
	return flushed
}

// RunUsageSpoolReplay drives recovery replay and ack-driven compaction:
//   - replay tick (default 30s): drains the WAL in back-to-back rounds while
//     progress is made (crash/DB-outage recovery; steady state relies on Ack
//   - compaction instead);
//   - compaction tick (2s) + threshold trigger: strips acked lines so the
//     WAL plateaus under sustained load.
func RunUsageSpoolReplay(ctx context.Context, spool *UsageSpool, interval time.Duration, flushBatch func(ctx context.Context, entries []*service.UsageEntry) error) {
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
			// interval per round. Progress is measured by the round's own
			// flushed count — NOT Pending(), which counts concurrent new
			// arrivals too and would falsely report "no progress" under live
			// traffic (a 160k-line backlog then drains at one round per tick).
			for round := 0; round < 200; round++ {
				flushed := spool.Replay(func(entries []*service.UsageEntry) error {
					fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					defer cancel()
					return flushBatch(fctx, entries)
				}, 200)
				if flushed == 0 || spool.Pending() == 0 {
					break // DB refusing rows (or nothing left but this round's acks)
				}
			}
		}
	}
}
