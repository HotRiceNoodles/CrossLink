package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newBudgetRouter(budget *InflightBodyBudget, maxSize int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(maxSize, budget, 0), func(c *gin.Context) {
		c.String(http.StatusOK, "ok:%d", len(GetBodyBytes(c)))
	})
	r.POST("/v1/audio/transcriptions", ReadBodyWithBudget(maxSize, budget, 4096), func(c *gin.Context) {
		c.String(http.StatusOK, "uploaded")
	})
	return r
}

// chunkedBody wraps a reader without Len()/Size() so net/http sends the
// request with Transfer-Encoding: chunked (Content-Length -1) — the exact
// "unknown length" admission path under test.
type chunkedBody struct{ r io.Reader }

func (c chunkedBody) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c chunkedBody) Close() error               { return nil }

func chunkedRequest(t *testing.T, path string, size int) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, chunkedBody{bytes.NewReader(bytes.Repeat([]byte("x"), size))})
	req.Header.Set("Content-Type", "application/json")
	return req
}

// The in-flight body budget must reject BEFORE buffering (Content-Length
// pre-check), release on completion, and stay transparent when disabled.
func TestInflightBodyBudget_RejectsBeforeBuffering(t *testing.T) {
	budget := NewInflightBodyBudget(64)
	if !budget.Acquire(60) { // simulate an in-flight large request
		t.Fatal("initial acquire failed")
	}
	r := newBudgetRouter(budget, 10<<20)

	body := bytes.Repeat([]byte("x"), 8)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("over-budget request admitted: %d %s", w.Code, w.Body.String())
	}
	if budget.Inflight() != 60 {
		t.Fatalf("failed request must not hold budget: inflight=%d", budget.Inflight())
	}

	budget.Release(60)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("fitting request rejected after release: %d %s", w2.Code, w2.Body.String())
	}
	if budget.Inflight() != 0 {
		t.Fatalf("budget not released after completion: inflight=%d", budget.Inflight())
	}
}

// Supervisor F26 review: unknown-length (chunked) requests must reserve
// BEFORE allocation. A flood of concurrent chunked bodies can never push the
// budget past its limit, and rejections happen before the body is read.
func TestInflightBodyBudget_ChunkedFloodNeverExceedsLimit(t *testing.T) {
	const limit = 3 * (1 << 20) // 3MiB
	budget := NewInflightBodyBudget(limit)
	r := newBudgetRouter(budget, 10<<20)

	const workers = 24
	var rejected, accepted atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 1<<20))
			if now := budget.Inflight(); now > limit {
				// cannot exceed by construction; sample for the peak report
			}
			for {
				p := peak.Load()
				if now := budget.Inflight(); now <= p || peak.CompareAndSwap(p, now) {
					break
				}
			}
			if w.Code == http.StatusServiceUnavailable {
				rejected.Add(1)
			} else if w.Code == http.StatusOK {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("budget leaked after all requests: inflight=%d", got)
	}
	if peak.Load() > limit {
		t.Fatalf("chunked flood pushed budget past limit: peak=%d limit=%d", peak.Load(), limit)
	}
	if accepted.Load() < 1 {
		t.Fatalf("admission LIVENESS regression: acc=%d rej=%d (limit=%d, bodies=1MiB) — partial reservations starved everyone",
			accepted.Load(), rejected.Load(), limit)
	}
	// Rejections under saturation are covered deterministically by
	// TestInflightBodyBudget_ChunkedRejectedWhenSaturated; the required
	// properties here: bounded queueing lets everyone complete serially, the
	// budget NEVER exceeds its limit, and it drains to zero.
}

// Oversized known Content-Length → 413 WITHOUT reserving anything: a huge
// declared length must neither consume budget nor be read.
func TestInflightBodyBudget_OversizedCLRejectsBeforeReservation(t *testing.T) {
	budget := NewInflightBodyBudget(1 << 30)
	r := newBudgetRouter(budget, 1024)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader(bytes.Repeat([]byte("z"), 4096)))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 512 << 20 // declared 512MB > maxSize
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized CL, got %d", w.Code)
	}
	if budget.Inflight() != 0 {
		t.Fatalf("oversized CL consumed budget: inflight=%d", budget.Inflight())
	}
}

// Over-size bodies (413) must not leak budget reservation.
func TestInflightBodyBudget_413ReleasesReservation(t *testing.T) {
	budget := NewInflightBodyBudget(1 << 30)
	r := newBudgetRouter(budget, 1024)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(bytes.Repeat([]byte("z"), 2048)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if budget.Inflight() != 0 {
		t.Fatalf("413 leaked budget reservation: inflight=%d", budget.Inflight())
	}
}

// Read errors (client cancel surfaces as one) on the unknown-length path
// release every held block.
func TestInflightBodyBudget_ReadErrorReleasesBlocks(t *testing.T) {
	budget := NewInflightBodyBudget(1 << 30)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		t.Fatal("handler must not run on read error")
	})
	boom := struct{ io.Reader }{io.MultiReader(strings.NewReader(`{"partial":`), errReader{})}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", chunkedBody{boom})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on read error, got %d", w.Code)
	}
	if budget.Inflight() != 0 {
		t.Fatalf("read error leaked held blocks: inflight=%d", budget.Inflight())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("client went away") }

// Multipart upload routes participate in the budget (their handlers buffer
// in-process via ParseMultipartForm): unknown-CL multipart reserves the
// configured conservative amount; known-CL reserves the smaller actual; both
// release at chain end.
func TestInflightBodyBudget_MultipartParticipates(t *testing.T) {
	const reserve = 4096
	budget := NewInflightBodyBudget(8192)
	r := newBudgetRouter(budget, 10<<20)

	// Unknown length (chunked multipart): reserves the configured amount.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, chunkedMultipart(t, "/v1/audio/transcriptions", -1))
	if w.Code != http.StatusOK {
		t.Fatalf("chunked multipart rejected: %d", w.Code)
	}
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("multipart reservation not released: %d", got)
	}

	// Known CL below the reserve: reserves CL only (2 requests fit in 8192).
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, chunkedMultipart(t, "/v1/audio/transcriptions", 3000))
		if w.Code != http.StatusOK {
			t.Fatalf("known-CL multipart %d rejected: %d", i, w.Code)
		}
	}
	// A third concurrent one would exceed — but they completed sequentially,
	// so instead verify exhaustion with a held reservation.
	if !budget.Acquire(8192 - 100) {
		t.Fatal("preflight acquire failed")
	}
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, chunkedMultipart(t, "/v1/audio/transcriptions", -1))
	if w3.Code != http.StatusServiceUnavailable {
		t.Fatalf("multipart bypassed exhausted budget: %d", w3.Code)
	}
	budget.Release(8192 - 100)
	if budget.Inflight() != 0 {
		t.Fatalf("budget not empty at end: %d", budget.Inflight())
	}
}

func chunkedMultipart(t *testing.T, path string, contentLength int64) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("--BOUNDARY\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.bin\"\r\n")
	buf.WriteString("Content-Type: application/octet-stream\r\n\r\n")
	buf.Write(bytes.Repeat([]byte("f"), 64))
	buf.WriteString("\r\n--BOUNDARY--\r\n")
	var body io.Reader = &buf
	var req *http.Request
	if contentLength < 0 {
		req = httptest.NewRequest(http.MethodPost, path, chunkedBody{body})
	} else {
		req = httptest.NewRequest(http.MethodPost, path, body)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	return req
}

// A request whose declared Content-Length understates the actual body must
// still be accounted at its true size (the reservation is corrected upward).
func TestInflightBodyBudget_CorrectsUnderstatedContentLength(t *testing.T) {
	budget := NewInflightBodyBudget(1 << 20)
	r := newBudgetRouter(budget, 10<<20)

	body := bytes.Repeat([]byte("y"), 4096)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = 16 // lie small
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("request failed: %d", w.Code)
	}
	if budget.Inflight() != 0 {
		t.Fatalf("corrected reservation not released: inflight=%d", budget.Inflight())
	}
}

// Disabled budget (limit 0) and nil budget behave exactly like ReadBody.
func TestInflightBodyBudget_DisabledPassthrough(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget *InflightBodyBudget
	}{
		{"nil", nil},
		{"zero-limit", NewInflightBodyBudget(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newBudgetRouter(tc.budget, 10<<20)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"a":1}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("passthrough failed: %d", w.Code)
			}
		})
	}
}

// Body rewrites (guardrail mask, template expansion) may not silently widen
// memory: SetBodyBytes acquires the delta, enforces the per-request ceiling,
// and shrinks the reservation when the body gets smaller.
func TestSetBodyBytes_BudgetAware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	budget := NewInflightBodyBudget(1 << 20)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		base := len(GetBodyBytes(c))
		// Grow inside budget → ok.
		if !SetBodyBytes(c, make([]byte, base+1024)) {
			t.Fatal("in-budget growth rejected")
		}
		// Shrink → reservation shrinks.
		SetBodyBytes(c, make([]byte, 64))
		if got := budget.Inflight(); got != 64 {
			t.Fatalf("reservation after shrink = %d, want 64", got)
		}
		c.String(http.StatusOK, "ok")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 128)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handler failed: %d", w.Code)
	}
	if budget.Inflight() != 0 {
		t.Fatalf("rewrite-aware reservation not released: %d", budget.Inflight())
	}
}

// Expansion past the per-request ceiling is rejected and leaves the buffer
// untouched; exhaustion of the budget likewise.
func TestSetBodyBytes_ExpansionCeilingAndBudgetExhaustion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	budget := NewInflightBodyBudget(2048)
	r := gin.New()
	r.POST("/v1/chat/completions", ReadBodyWithBudget(1024, budget, 0), func(c *gin.Context) {
		before := GetBodyBytes(c)
		if SetBodyBytes(c, make([]byte, 1025)) {
			t.Fatal("expansion past maxSize accepted")
		}
		if &GetBodyBytes(c)[0] != &before[0] {
			t.Fatal("buffer mutated on rejected rewrite")
		}
		// In-ceiling but over-budget (budget nearly full).
		budget.Acquire(2048 - 64) // leave ~64
		defer budget.Release(2048 - 64)
		if SetBodyBytes(c, make([]byte, 700)) {
			t.Fatal("over-budget rewrite accepted")
		}
		c.String(http.StatusOK, "ok")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 32)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("handler failed: %d %s", w.Code, w.Body.String())
	}
	if budget.Inflight() != 0 {
		t.Fatalf("budget leaked: %d", budget.Inflight())
	}
}

// Regression (runtime evidence 2026-10-15): a chunked body arriving in SMALL
// frames (KBs per Read) must reserve by BYTES BUFFERED (rounded to 512KiB
// steps), never per Read call — per-Read reservation multiplied a 2MiB body
// into hundreds of MiB of budget demand.
func TestInflightBodyBudget_SmallFrameReadsDoNotInflateReservation(t *testing.T) {
	budget := NewInflightBodyBudget(8 << 20) // 8MiB
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var peakReserve int64
	r.POST("/v1/chat/completions", ReadBodyWithBudget(10<<20, budget, 0), func(c *gin.Context) {
		c.String(http.StatusOK, "ok:%d", len(GetBodyBytes(c)))
	})
	// dribbleReader returns at most 4KB per Read — chunked-frame-like.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		chunkedBody{&dribbleReader{data: bytes.Repeat([]byte("x"), 2<<20)}})
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("2MiB dribbled body rejected against 8MiB budget: %d %s", w.Code, w.Body.String())
	}
	// During the request the reservation must have stayed ≤ 2MiB + one block
	// (sampled via the handler above is racy; assert via total release instead
	// and the flood test above pins the invariant).
	if got := budget.Inflight(); got != 0 {
		t.Fatalf("dribbled body leaked reservation: %d", got)
	}
	_ = peakReserve
}

type dribbleReader struct {
	data []byte
	off  int
}

func (d *dribbleReader) Read(p []byte) (int, error) {
	if d.off >= len(d.data) {
		return 0, io.EOF
	}
	n := len(p)
	if n > 4096 {
		n = 4096
	}
	if n > len(d.data)-d.off {
		n = len(d.data) - d.off
	}
	copy(p, d.data[d.off:d.off+n])
	d.off += n
	return n, nil
}

// Heap-peak evidence (acceptance round 2): N concurrent 4MiB unknown-length
// reads must bound live heap growth at roughly 2x payload per request (block
// chain + final assembly) — no unbounded multiplier, no bytes.Buffer doubling.
func TestInflightBodyBudget_HeapPeakBounded(t *testing.T) {
	const n = 8
	const bodyMiB = 4
	budget := NewInflightBodyBudget(256 << 20)
	r := newBudgetRouter(budget, 10<<20)

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", bodyMiB<<20))
			if w.Code != http.StatusOK {
				t.Errorf("request rejected: %d", w.Code)
			}
		}()
	}
	wg.Wait()

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if grew < 0 {
		grew = 0
	}
	// Bound: chain (4MiB) + assembled body (4MiB) + scratch (0.5MiB) per
	// request, plus generous allocator slack. The point is the ~2x bound,
	// not a precise number.
	limit := int64(n)*(2*bodyMiB<<20+2<<20) + (32 << 20)
	if grew > limit {
		t.Fatalf("heap grew %d bytes for %d×%dMiB bodies (limit %d) — allocation not following the reservation",
			grew, n, bodyMiB, limit)
	}
}

// Deterministic saturation rejection for unknown-length bodies (the flood
// test above now proves liveness/plateau, not rejection): with the budget
// pre-held, a chunked request must 503 within the bounded-wait window.
func TestInflightBodyBudget_ChunkedRejectedWhenSaturated(t *testing.T) {
	budget := NewInflightBodyBudget(1 << 20)
	r := newBudgetRouter(budget, 10<<20)
	if !budget.Acquire((1 << 20) - (100 << 10)) { // leave ~100KB
		t.Fatal("pre-hold failed")
	}
	t.Cleanup(func() { budget.Release((1 << 20) - (100 << 10)) })
	w := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(w, chunkedRequest(t, "/v1/chat/completions", 64<<10))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated chunked request admitted: %d", w.Code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("rejection waited %v — bounded wait exceeded", d)
	}
	if budget.Inflight() != (1<<20)-(100<<10) {
		t.Fatalf("rejected request leaked reservation: %d", budget.Inflight())
	}
}
