package handler

// Supervisor fault review (acceptance round 4): ANY failure inside Compact
// — non-EOF read error, write/flush failure, fsync failure, rename failure —
// must keep the ORIGINAL WAL byte-identical, remove the temp file, and leave
// the ack snapshot unconsumed (consuming acks on failure silently regresses
// throughput to replay-only stripping). Uncommitted events must survive
// every fault. Faults are injected via the rewriteStream io seams and the
// walRename/walSync OS seams.

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/service"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func faultSpool(t *testing.T) (*UsageSpool, string) {
	t.Helper()
	dir := t.TempDir()
	s := NewUsageSpool(dir, 64<<20)
	t.Cleanup(s.Close)
	if s == nil {
		t.Fatal("spool not armed")
	}
	return s, dir
}

func walBytes(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(spoolFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ackSnapshot(s *UsageSpool) int {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	return len(s.acked)
}

// errAfterReader yields ok full reads then fails with a NON-EOF error.
type errAfterReader struct {
	data []byte
	off  int
	ok   int
	n    int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.n >= r.ok {
		return 0, errors.New("disk read I/O error")
	}
	r.n++
	if r.off >= len(r.data) {
		return 0, nil //nolint: caller treats EOF via ReadBytes
	}
	c := copy(p, r.data[r.off:])
	r.off += c
	return c, nil
}

// A mid-file NON-EOF read error aborts the rewrite: original kept, temp
// gone, acks retained; the next (healthy) compaction strips correctly.
func TestCompact_NonEOFReadErrorKeepsOriginalAndAcks(t *testing.T) {
	s, dir := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "e1"})
	s.Write(&service.UsageEntry{EventID: "e2"})
	before := walBytes(t, dir)
	s.Ack("e1")

	src := &errAfterReader{data: before, ok: 0} // fails on FIRST read
	var out bytes.Buffer
	w := bufio.NewWriter(&out)
	if _, _, err := s.rewriteStream(src, w, map[string]struct{}{"e1": {}}); err == nil {
		t.Fatal("read error not surfaced")
	}
	if ackSnapshot(s) != 1 {
		t.Fatalf("ack consumed on failed rewrite: %d left", ackSnapshot(s))
	}
	if got := walBytes(t, dir); !bytes.Equal(got, before) {
		t.Fatal("original WAL modified by failed rewrite")
	}
	// Healthy compaction now strips e1 and keeps e2.
	if removed := s.Compact(); removed != 1 {
		t.Fatalf("healthy compact removed %d, want 1", removed)
	}
	if strings.Contains(string(walBytes(t, dir)), "e2") != true {
		t.Fatal("uncommitted e2 lost")
	}
}

// A write failure mid-rewrite aborts: same guarantees.
type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after <= 0 {
		return 0, errors.New("disk full (ENOSPC)")
	}
	w.after--
	return len(p), nil
}

func TestCompact_WriteFailureKeepsOriginalAndAcks(t *testing.T) {
	s, dir := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "keep1"})
	s.Write(&service.UsageEntry{EventID: "keep2"})
	before := walBytes(t, dir)
	s.Ack("keep1")

	var buf bytes.Buffer
	buf.Write(before) // reader source
	src := bytes.NewReader(before)
	w := bufio.NewWriter(&failWriter{after: 0})
	if _, _, err := s.rewriteStream(src, w, map[string]struct{}{"keep1": {}}); err == nil {
		t.Fatal("write error not surfaced")
	}
	if ackSnapshot(s) != 1 {
		t.Fatalf("ack consumed on write failure: %d left", ackSnapshot(s))
	}
	if !bytes.Equal(walBytes(t, dir), before) {
		t.Fatal("original WAL modified by failed write")
	}
}

// fsync failure (OS seam): original kept, temp removed, acks retained.
func TestCompact_SyncFailureKeepsOriginalAndAcks(t *testing.T) {
	s, dir := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "a"})
	s.Write(&service.UsageEntry{EventID: "b"})
	before := walBytes(t, dir)
	s.Ack("a")

	oldSync := walSync
	walSync = func(f *os.File) error { return errors.New("fsync failed") }
	t.Cleanup(func() { walSync = oldSync })
	if removed := s.Compact(); removed != 0 {
		t.Fatalf("sync-failed compact reported removed=%d", removed)
	}
	if ackSnapshot(s) != 1 {
		t.Fatalf("ack consumed on sync failure: %d left", ackSnapshot(s))
	}
	if !bytes.Equal(walBytes(t, dir), before) {
		t.Fatal("original WAL modified by failed fsync")
	}
	if _, err := os.Stat(spoolFile(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind after failed fsync")
	}
	// Recovery: healthy compaction succeeds.
	walSync = oldSync
	if removed := s.Compact(); removed != 1 {
		t.Fatalf("recovery compact removed %d, want 1", removed)
	}
}

// rename failure (OS seam): same guarantees.
func TestCompact_RenameFailureKeepsOriginalAndAcks(t *testing.T) {
	s, dir := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "x"})
	s.Write(&service.UsageEntry{EventID: "y"})
	before := walBytes(t, dir)
	s.Ack("x")

	oldRename := walRename
	walRename = func(old, new string) error { return errors.New("rename failed (EBUSY)") }
	t.Cleanup(func() { walRename = oldRename })
	if removed := s.Compact(); removed != 0 {
		t.Fatalf("rename-failed compact reported removed=%d", removed)
	}
	if ackSnapshot(s) != 1 {
		t.Fatalf("ack consumed on rename failure: %d left", ackSnapshot(s))
	}
	if !bytes.Equal(walBytes(t, dir), before) {
		t.Fatal("original WAL modified by failed rename")
	}
	walRename = oldRename
	if removed := s.Compact(); removed != 1 {
		t.Fatalf("recovery compact removed %d, want 1", removed)
	}
}

// Torn tail + corrupt line: quarantined to the sidecar and counted ONCE (at
// Compact removal); healthy lines around them survive.
func TestCompact_QuarantineSingleCount(t *testing.T) {
	s, dir := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "good"})
	f, _ := os.OpenFile(spoolFile(dir), os.O_WRONLY|os.O_APPEND, 0o644)
	f.WriteString("this-is-not-json\n") // corrupt
	f.WriteString(`{"EventID":"torn"`)  // torn tail (no newline)
	f.Close()

	s.Ack("good")
	before := testutil.ToFloat64(usageSpoolCorruptTotal)
	removed := s.Compact()
	if removed != 1 {
		t.Fatalf("removed %d, want 1 (good only)", removed)
	}
	after := testutil.ToFloat64(usageSpoolCorruptTotal)
	if after-before != 2 { // corrupt + torn each counted exactly once
		t.Fatalf("corrupt counter delta = %v, want 2", after-before)
	}
	q, err := os.ReadFile(spoolFile(dir) + ".corrupt")
	if err != nil || !bytes.Contains(q, []byte("this-is-not-json")) || !bytes.Contains(q, []byte(`{"EventID":"torn"`)) {
		t.Fatalf("quarantine sidecar missing lines: %v %q", err, q)
	}
	// Uncommitted good line was acked+removed; WAL now empty.
	if s.Pending() != 0 {
		t.Fatalf("WAL not empty: %d", s.Pending())
	}
}

// Legacy pre-EventID lines are LEGAL billing rows: Replay flushes them and
// acks under the synthetic key; Compact strips them — never silently lost.
func TestReplay_LegacyLineFlushedAndStripped(t *testing.T) {
	s, dir := faultSpool(t)
	legacy := `{"RouteType":"openai","OutputTokens":7}` + "\n"
	os.WriteFile(spoolFile(dir), []byte(legacy), 0o644)

	flushed := 0
	s.Replay(perRow(func(e *service.UsageEntry) error {
		flushed++
		if e.OutputTokens != 7 {
			t.Fatalf("legacy row corrupted: %+v", e)
		}
		return nil
	}), 10)
	if flushed != 1 {
		t.Fatalf("legacy line not flushed: %d", flushed)
	}
	// Replay acks the synthetic key AND runs its internal compaction, so
	// the line is already stripped by the time Replay returns.
	if s.Pending() != 0 {
		t.Fatalf("legacy line not stripped by replay+compact: %d left", s.Pending())
	}
	if ackSnapshot(s) != 0 {
		t.Fatalf("acks not consumed by successful strip: %d", ackSnapshot(s))
	}
	if s.Pending() != 0 {
		t.Fatalf("WAL not empty after legacy strip: %d", s.Pending())
	}
}

// Concurrent ack DURING a failed compaction must survive it: the next
// compaction strips both the snapshot and the late ack.
func TestCompact_ConcurrentAckSurvivesFailedCompact(t *testing.T) {
	s, _ := faultSpool(t)
	s.Write(&service.UsageEntry{EventID: "early"})
	s.Write(&service.UsageEntry{EventID: "late"})
	s.Ack("early")

	oldSync := walSync
	failed := false
	walSync = func(f *os.File) error {
		if !failed {
			failed = true
			return errors.New("one-shot fsync failure")
		}
		return oldSync(f)
	}
	t.Cleanup(func() { walSync = oldSync })

	if removed := s.Compact(); removed != 0 {
		t.Fatalf("injected failure not hit: removed=%d", removed)
	}
	// Late ack arrives AFTER the failed compaction.
	s.Ack("late")
	if ackSnapshot(s) != 2 {
		t.Fatalf("acks after failure: %d, want 2", ackSnapshot(s))
	}
	if removed := s.Compact(); removed != 2 {
		t.Fatalf("recovery compact removed %d, want 2", removed)
	}
	if s.Pending() != 0 {
		t.Fatal("uncommitted events lost")
	}
	_ = time.Second
}
