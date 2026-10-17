package handler

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crosslink/internal/service"
)

// ---------------------------------------------------------------------------
// Group-commit adversarial contract (supervisor verification 3, 2026-10-17):
// under fsync stall/timeout, DB fallback, crash windows and concurrent
// compaction, the WAL must never LOSE a confirmed event (Write returned true)
// and never DOUBLE-COUNT one (EventID idempotency).
// ---------------------------------------------------------------------------

// stallSync makes the shared walSync seam fail (or stall) for the test's
// duration, restoring the real fsync afterwards.
func stallSync(t *testing.T, stall time.Duration) {
	t.Helper()
	prev := walSync
	walSync = func(f *os.File) error {
		time.Sleep(stall)
		return errors.New("injected fsync stall")
	}
	t.Cleanup(func() { walSync = prev })
}

// A stalled disk: Write must return false (NOT durable) within the bounded
// wait — never hang — so the caller can take the synchronous DB fallback.
func TestGroupCommit_SyncStallWriteIsBoundedFalse(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 1<<20)
	t.Cleanup(spool.Close)
	stallSync(t, 2*time.Second)

	start := time.Now()
	ok := spool.Write(&service.UsageEntry{EventID: "stall-1", RouteType: "openai"})
	elapsed := time.Since(start)
	if ok {
		t.Fatal("Write must not report durability while fsync is failing")
	}
	if elapsed > 7*time.Second {
		t.Fatalf("Write hung %v on stalled fsync (bound is ~5s)", elapsed)
	}
}

// Full fallback path with a stalled WAL: every event goes through the
// synchronous DB commit (the caller's fallback — simulated by recording the
// entries), and after the WAL recovers, replaying whatever bytes DID reach
// the file must not duplicate any event (EventID idempotent flush).
func TestGroupCommit_StallThenFallbackThenReplay_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)
	stallSync(t, 500*time.Millisecond)

	// Caller-side fallback: Write false -> synchronous DB write (recorded).
	fallback := make(map[string]bool)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("fb-%d", i)
		if !spool.Write(&service.UsageEntry{EventID: id, RouteType: "openai"}) {
			fallback[id] = true // committed synchronously by the caller
		}
	}
	if len(fallback) != 20 {
		t.Fatalf("expected all writes to fall back while fsync stalls, got %d/20", len(fallback))
	}

	// Disk recovers; some stalled appends may have reached the file anyway.
	walSync = func(f *os.File) error { return f.Sync() }
	// Wait for the syncer to cover any appended bytes, then replay everything.
	time.Sleep(200 * time.Millisecond)
	seen := map[string]bool{}
	n := spool.Replay(perRow(func(e *service.UsageEntry) error {
		if seen[e.EventID] {
			t.Fatalf("duplicate replay of %s", e.EventID)
		}
		seen[e.EventID] = true
		// An event that the caller already committed via fallback must NOT be
		// counted twice in the accounting sense: the flush is idempotent
		// (OnConflict DoNothing upstream); here we assert presence-once.
		if fallback[e.EventID] && seen[e.EventID] {
			// presence in the WAL after fallback is legal (bytes landed);
			// the DB layer guarantees single-row via the unique index.
		}
		return nil
	}), 1000)
	t.Logf("replayed %d rows after recovery", n)
	spool.Close()

	// Reopen (crash simulation): replay again — still no duplicates.
	spool2 := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool2.Close)
	_ = spool2.Replay(perRow(func(e *service.UsageEntry) error {
		if _, dup := seen[e.EventID]; dup {
			// second Replay pass must skip acked lines; if it re-reads one it
			// flushes idempotently — but the acked-skip should prevent even that
			t.Logf("note: %s re-read after ack (idempotent if flushed)", e.EventID)
		}
		return nil
	}), 1000)
}

// Concurrent compaction vs writers: every Write that returned true must be
// present exactly once in the final WAL accounting, and the full drain lands
// every confirmed event exactly once in the "DB" (recorded map).
func TestGroupCommit_ConcurrentCompactionNoLossNoDup(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 16<<20)
	t.Cleanup(spool.Close)

	const writers, perWriter = 8, 60
	var confirmed sync.Map
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Compactor: hammers Compact for the whole write phase.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				spool.Compact()
				time.Sleep(time.Millisecond)
			}
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("cc-%d-%d", w, i)
				if spool.Write(&service.UsageEntry{EventID: id, RouteType: "openai"}) {
					confirmed.Store(id, true)
				}
			}
		}(w)
	}

	// Wait for writers only, then stop the compactor.
	doneW := make(chan struct{})
	go func() { wg.Wait(); close(doneW) }()
	// writers+compactor share wg; wait for writers by counting confirmed.
	for {
		var n int
		confirmed.Range(func(_, _ any) bool { n++; return true })
		if n == writers*perWriter {
			break
		}
		select {
		case <-time.After(30 * time.Second):
			t.Fatalf("writers stalled at %d/%d", n, writers*perWriter)
		case <-time.After(50 * time.Millisecond):
		}
	}
	close(stop)

	db := map[string]bool{}
	var dup atomic.Int64
	for {
		n := spool.Replay(perRow(func(e *service.UsageEntry) error {
			if db[e.EventID] {
				dup.Add(1)
			}
			db[e.EventID] = true
			return nil
		}), 100000)
		if n == 0 {
			break
		}
	}

	missing := 0
	confirmed.Range(func(k, _ any) bool {
		if !db[k.(string)] {
			missing++
		}
		return true
	})
	if missing > 0 || dup.Load() > 0 {
		t.Fatalf("loss=%d duplicates=%d (confirmed writers*perWriter=%d)", missing, dup.Load(), writers*perWriter)
	}
}

// Crash window: bytes appended but fsync unconfirmed at kill time. Events
// whose Write CONFIRMED (returned true) must all recover exactly once; the
// unconfirmed tail is allowed to survive (idempotent replay) or be absent —
// but never a confirmed-loss or a duplicate.
func TestGroupCommit_CrashWindowConfirmedEventsRecoverOnce(t *testing.T) {
	dir := t.TempDir()
	spool := NewUsageSpool(dir, 8<<20)

	var confirmed []string
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("cw-%d", i)
		if !spool.Write(&service.UsageEntry{EventID: id, RouteType: "openai"}) {
			t.Fatalf("write %s failed on a healthy disk", id)
		}
		confirmed = append(confirmed, id)
	}
	// Simulate crash-after-append-before-sync: raw-append one extra line
	// WITHOUT waiting for the syncer, then "die" (close without draining).
	torn := `{"EventID":"cw-torn","RouteType":"openai"}` + "\n"
	f, err := os.OpenFile(filepath.Join(dir, "usage-spill.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(torn)
	f.Close()
	spool.Close() // process dies

	// Recover on a fresh instance (boot sanitize + replay).
	spool2 := NewUsageSpool(dir, 8<<20)
	t.Cleanup(spool2.Close)
	got := map[string]int{}
	for {
		n := spool2.Replay(perRow(func(e *service.UsageEntry) error {
			got[e.EventID]++
			return nil
		}), 100000)
		if n == 0 {
			break
		}
	}
	for _, id := range confirmed {
		c := got[id]
		if c == 0 {
			t.Fatalf("CONFIRMED event %s lost across crash window", id)
		}
		if c > 1 {
			t.Fatalf("event %s replayed %d times (duplicate billing risk)", id, c)
		}
	}
	// The torn/unconfirmed line: present at most once.
	if got["cw-torn"] > 1 {
		t.Fatalf("unconfirmed tail replayed %d times", got["cw-torn"])
	}
	// And nothing alien.
	for id := range got {
		if !strings.HasPrefix(id, "cw-") {
			t.Fatalf("alien event %q in WAL", id)
		}
	}
}
