package middleware

// Deterministic barrier regressions for the unknown-length (chunked) admission
// redesign (partial-reservation tiling livelock, 2026-10-18 P1).
//
// The livelock being excluded: N unknown readers each hold one 512KiB first
// block while waiting (bounded) for the NEXT block; when N×block tiles the
// whole budget nobody can complete and every waiter eventually times out —
// acc=0 although serial admission was possible. The fix is structural (slot
// gate sized so the worst collective read footprint fits the budget; growth
// above the per-request ceiling is a deterministic 413), so these tests prove
// progress WITHOUT extending the 500ms bound or weakening assertions: the
// admitted reader completes with no contention from other unknown readers by
// construction, on any machine speed.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// trackPeak samples the budget reservation until done closes, keeping the
// maximum observed value (the safety invariant sample).
func trackPeak(b *InflightBodyBudget, done <-chan struct{}) *atomic.Int64 {
	var peak atomic.Int64
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			for {
				p := peak.Load()
				now := b.Inflight()
				if now <= p || peak.CompareAndSwap(p, now) {
					break
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return &peak
}

// THE barrier: six unknown readers whose six 512KiB first-blocks tile a 3MiB
// budget exactly — the minimal old-scheme livelock configuration. Small
// bodies (64KiB) still tile because reservation granularity is one block.
// Under the slot gate K=1: the six reads serialize and ALL complete, on any
// machine speed (each read is an in-memory copy, uncontended by budget).
func TestUnknownAdmission_TilingLivelockExcluded(t *testing.T) {
	const limit = 3 << 20
	budget := NewInflightBodyBudget(limit)
	r := newBudgetRouter(budget, 10<<20) // ceiling = (3MiB-0.5MiB)/2 = 1.25MiB, K = 1

	const workers = 6
	done := make(chan struct{})
	peak := trackPeak(budget, done)
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 64<<10))
			if w.Code == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	close(done)

	if got := ok.Load(); got != workers {
		t.Fatalf("tiling livelock NOT excluded: only %d/%d unknown readers completed (old scheme: first-blocks tile the budget, everyone times out)", got, workers)
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("budget exceeded: peak=%d limit=%d", p, limit)
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked: inflight=%d", got)
	}
}

// Slow small uploads (paced 4KiB frames): liveness means the ADMITTED reader
// always finishes; a livelock yields zero completions. With K readers
// admitted and bodies within the ceiling, those K complete regardless of
// pacing; the rest may legitimately saturate (503) — never hang.
func TestUnknownAdmission_SlowSmallUploadsMakeProgress(t *testing.T) {
	const limit = 8 << 20
	budget := NewInflightBodyBudget(limit)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		c.String(http.StatusOK, "ok:%d", len(GetBodyBytes(c)))
	})

	// One paced upload alone must complete in full (no contention at all).
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		chunkedBody{&pacedReader{data: bytes.Repeat([]byte("x"), 256<<10), frame: 4 << 10, pause: 2 * time.Millisecond}}))
	if w.Code != http.StatusOK {
		t.Fatalf("single slow upload failed: %d %s", w.Code, w.Body.String())
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("slow upload leaked reservation: %d", got)
	}

	// A paced flood: the admitted reader completes (progress); everyone else
	// is bounded (200 or 503, never a hang); budget drains to zero.
	const workers = 4
	done := make(chan struct{})
	peak := trackPeak(budget, done)
	var ok, rej atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				chunkedBody{&pacedReader{data: bytes.Repeat([]byte("x"), 256<<10), frame: 4 << 10, pause: 2 * time.Millisecond}}))
			switch w.Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusServiceUnavailable:
				rej.Add(1)
			default:
				t.Errorf("unexpected status: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	close(done)
	if ok.Load() < 1 {
		t.Fatalf("no progress under paced flood: acc=%d rej=%d (livelock regression)", ok.Load(), rej.Load())
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("budget exceeded: peak=%d limit=%d", p, limit)
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

// Cancel/timeout: a reader stuck mid-body (client goes away) must release
// BOTH its budget blocks and its slot — the next unknown reader proceeds.
// Coordination is channel-based; no timing assumptions.
func TestUnknownAdmission_CancelFreesSlotAndBudget(t *testing.T) {
	const limit = 3 << 20
	budget := NewInflightBodyBudget(limit)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		c.String(http.StatusOK, "ok:%d", len(GetBodyBytes(c)))
	})

	gate := make(chan struct{})
	var closeGate sync.Once
	shutdown := func() { closeGate.Do(func() { close(gate) }) }
	defer shutdown()
	stuck := &gateReader{first: bytes.Repeat([]byte("x"), 4<<10), gate: gate}

	var wg1 sync.WaitGroup
	wg1.Add(1)
	go func() {
		defer wg1.Done()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chunkedBody{stuck}))
		if w.Code != http.StatusBadRequest {
			t.Errorf("stuck reader should surface read error as 400, got %d", w.Code)
		}
	}()

	// Wait until the stuck reader holds its entry block, then fire the second
	// reader — it queues for the slot (K=1) until the cancel frees it.
	deadline := time.Now().Add(5 * time.Second)
	for budget.Inflight() < readBodyBlock && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if budget.Inflight() < readBodyBlock {
		t.Fatal("stuck reader never acquired its entry block")
	}

	shutdown() // client goes away -> read error -> 400 + release
	wg1.Wait()

	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() {
		defer wg2.Done()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 64<<10))
		if w.Code != http.StatusOK {
			t.Errorf("post-cancel unknown reader rejected (slot/budget not freed): %d %s", w.Code, w.Body.String())
		}
	}()
	wg2.Wait()
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

// Known-CL and chunked mixed: known-CL holds its (never-waiting) reservation
// through the handler while chunked readers are admitted; both kinds make
// progress, the sampled peak stays within the limit, everything drains.
func TestUnknownAdmission_MixedKnownCLAndChunked(t *testing.T) {
	const limit = 4 << 20
	budget := NewInflightBodyBudget(limit)
	gin.SetMode(gin.TestMode)
	hold := make(chan struct{})
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		if c.GetHeader("X-Kind") == "known" {
			<-hold // keep the known-CL reservation live across the chunked phase
		}
		c.String(http.StatusOK, "ok:%d", len(GetBodyBytes(c)))
	})

	done := make(chan struct{})
	peak := trackPeak(budget, done)

	var knownOK, chunkedOK atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				bytes.NewReader(bytes.Repeat([]byte("k"), 512<<10)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Kind", "known")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code == http.StatusOK {
				knownOK.Add(1)
			}
		}()
	}
	// Let the known reservations land before firing the chunked readers.
	deadline := time.Now().Add(5 * time.Second)
	for budget.Inflight() < 2*(512<<10) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 512<<10))
			if w.Code == http.StatusOK {
				chunkedOK.Add(1)
			}
		}()
	}
	// Release the known holders only after the chunked readers have had their
	// entry round: with K=1 they serialize but never wait on the knowns
	// (growth fits beside the known reservations: 2×0.5MiB+0.5MiB each,
	// 1MiB known + 1.5MiB reader = 2.5MiB ≤ 4MiB).
	time.Sleep(50 * time.Millisecond)
	close(hold)
	wg.Wait()
	close(done)

	if knownOK.Load() != 2 {
		t.Fatalf("known-CL progress broken: %d/2", knownOK.Load())
	}
	if chunkedOK.Load() < 1 {
		t.Fatalf("chunked progress broken under mixed load: %d/3", chunkedOK.Load())
	}
	if p := peak.Load(); p > limit {
		t.Fatalf("budget exceeded: peak=%d limit=%d", p, limit)
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

// A body over the per-request ceiling can NEVER be served under this budget
// (its 2×payload+scratch peak cannot fit) — it must get a deterministic 413
// immediately, never a queue-then-timeout.
func TestUnknownAdmission_OverCeilingIsDeterministic413(t *testing.T) {
	const limit = 3 << 20
	budget := NewInflightBodyBudget(limit)
	r := newBudgetRouter(budget, 10<<20) // ceiling 1.25MiB

	start := time.Now()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 2<<20))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-ceiling body must 413, got %d %s", w.Code, w.Body.String())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("over-ceiling rejection queued instead of failing fast: %s", el)
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("413 leaked budget: %d", got)
	}
}

// limit < maxSize must NOT starve small legal requests (the naive
// slots=floor(limit/maxSize) rounds to zero and rejects everything): the
// ceiling shrinks to fit the budget, K floors at 1, small bodies complete.
func TestUnknownAdmission_LimitBelowMaxSizeSmallRequestsUsable(t *testing.T) {
	const limit = 3 << 20
	budget := NewInflightBodyBudget(limit)
	r := newBudgetRouter(budget, 10<<20)

	for i := 0; i < 10; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 100<<10))
		if w.Code != http.StatusOK {
			t.Fatalf("small chunked request %d rejected under limit<maxSize: %d %s", i, w.Code, w.Body.String())
		}
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked: %d", got)
	}
}

// pacedReader delivers at most `frame` bytes per Read with a pause — a slow
// chunked uploader.
type pacedReader struct {
	data  []byte
	off   int
	frame int
	pause time.Duration
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.off >= len(p.data) {
		return 0, io.EOF
	}
	n := len(b)
	if n > p.frame {
		n = p.frame
	}
	if n > len(p.data)-p.off {
		n = len(p.data) - p.off
	}
	copy(b, p.data[p.off:p.off+n])
	p.off += n
	time.Sleep(p.pause)
	return n, nil
}

// gateReader delivers one chunk then blocks until its gate closes, when it
// errors — a client that goes away mid-body.
type gateReader struct {
	first []byte
	gate  chan struct{}
	once  bool
}

func (g *gateReader) Read(p []byte) (int, error) {
	if !g.once {
		g.once = true
		return copy(p, g.first), nil
	}
	<-g.gate
	return 0, errors.New("client went away")
}
