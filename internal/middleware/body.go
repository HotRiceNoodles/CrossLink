package middleware

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

const BodyKey = "_body_bytes"

const bodyReservationKey = "_body_reservation"

// InflightBodyBudget caps the total bytes of buffered request bodies across
// in-flight gateway requests. The concurrency limit bounds request COUNT, not
// memory: N slots x the per-request body ceiling is GiBs of BodyKey buffers
// held for the whole request lifetime (including SSE streaming). A budget
// rejects new requests once the cap is reached — BEFORE their body is
// buffered — so admission stays ahead of the expensive buffering/parse
// stages.
type InflightBodyBudget struct {
	limit    int64
	inflight atomic.Int64

	// Unknown-length (chunked) reads are gated by SLOTS so their collective
	// worst-case footprint always fits the budget (see ReadBodyWithBudget).
	// unkCeil is the largest per-request ceiling any route registered; the
	// slot channel is (re)built under unkMu whenever that value changes.
	unkMu    sync.Mutex
	unkCeil  atomic.Int64
	unkSlots chan struct{}
	unkBuilt int64 // ceiling the current channel was sized for
}

// activeBodyBudget + budgetMetricsOnce expose the effective budget through
// /metrics: cl_gateway_inflight_body_bytes / cl_gateway_inflight_body_budget_
// limit_bytes. Registered once; the gauges follow the latest created budget
// (production creates exactly one; tests may create several).
var (
	activeBodyBudget  atomic.Pointer[InflightBodyBudget]
	budgetMetricsOnce sync.Once
)

func NewInflightBodyBudget(limit int64) *InflightBodyBudget {
	b := &InflightBodyBudget{limit: limit}
	activeBodyBudget.Store(b)
	budgetMetricsOnce.Do(func() {
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "cl_gateway_inflight_body_bytes",
				Help: "Bytes currently reserved by the in-flight request-body budget.",
			},
			func() float64 {
				if cur := activeBodyBudget.Load(); cur != nil {
					return float64(cur.inflight.Load())
				}
				return 0
			},
		))
		prometheus.MustRegister(prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "cl_gateway_inflight_body_budget_limit_bytes",
				Help: "Configured in-flight body budget limit (0 = disabled) — the runtime proof a configured cap is actually in effect.",
			},
			func() float64 {
				if cur := activeBodyBudget.Load(); cur != nil {
					return float64(cur.limit)
				}
				return 0
			},
		))
	})
	return b
}

// Acquire reserves n bytes; false means the budget is exhausted.
func (b *InflightBodyBudget) Acquire(n int64) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	for {
		cur := b.inflight.Load()
		if cur+n > b.limit {
			return false
		}
		if b.inflight.CompareAndSwap(cur, cur+n) {
			return true
		}
	}
}

// Release returns n bytes to the budget.
func (b *InflightBodyBudget) Release(n int64) {
	if b == nil || b.limit <= 0 {
		return
	}
	for {
		cur := b.inflight.Load()
		next := cur - n
		if next < 0 {
			next = 0
		}
		if b.inflight.CompareAndSwap(cur, next) {
			return
		}
	}
}

// admissionWait bounds how long an unknown-length request may queue for a
// slot or budget before 503. It is a saturation backstop ONLY — liveness
// never depends on it (see ReadBodyWithBudget): an admitted reader completes
// without budget contention from other unknown readers by construction.
const admissionWait = 500 * time.Millisecond

// registerUnknownCeiling records the per-request ceiling of a route reading
// unknown-length bodies (called at middleware construction, i.e. startup).
// The slot gate is sized from the WORST (largest) ceiling so the collective
// bound K * (2C + block) <= limit holds across routes.
func (b *InflightBodyBudget) registerUnknownCeiling(ceil int64) {
	if b == nil || b.limit <= 0 || ceil <= 0 {
		return
	}
	for {
		cur := b.unkCeil.Load()
		if ceil <= cur || b.unkCeil.CompareAndSwap(cur, ceil) {
			return
		}
	}
}

// unknownSlots returns the gate for unknown-length reads. Rebuilding on a
// newly-registered larger ceiling starts a fresh (empty) channel — old slot
// holders drain into the garbage channel — which can only OVER-admit in the
// pathological "route registered after traffic started" case; safety never
// depends on the gate (Acquire enforces the cap), only liveness does.
func (b *InflightBodyBudget) unknownSlots() chan struct{} {
	ceil := b.unkCeil.Load()
	b.unkMu.Lock()
	defer b.unkMu.Unlock()
	if b.unkSlots != nil && b.unkBuilt == ceil {
		return b.unkSlots
	}
	k := b.limit / (2*ceil + readBodyBlock)
	if k < 1 {
		k = 1
	}
	b.unkSlots = make(chan struct{}, k)
	b.unkBuilt = ceil
	return b.unkSlots
}

// AcquireWait is Acquire with bounded queueing (acceptance round 3 —
// admission LIVENESS): under a flood of unknown-length requests, immediate
// failure lets partial reservations (first-block holders) saturate the
// budget and starve EVERYONE — acc=0 while serial progress was possible.
// Waiters poll briefly so completing requests release their blocks and the
// queue drains in arrival order; genuinely saturated budgets still reject
// after the bound. Returns false only when the budget stayed exhausted.
func (b *InflightBodyBudget) AcquireWait(n int64, wait time.Duration) bool {
	if b == nil || b.limit <= 0 {
		return true
	}
	if b.Acquire(n) {
		return true
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		if b.Acquire(n) {
			return true
		}
	}
	return false
}

// Inflight reports the currently reserved bytes.
func (b *InflightBodyBudget) Inflight() int64 {
	if b == nil {
		return 0
	}
	return b.inflight.Load()
}

func ReadBody(maxSize int64) gin.HandlerFunc {
	return ReadBodyWithBudget(maxSize, nil, 0)
}

// bodyReservation tracks ONE request's share of the in-flight body budget and
// the per-request expansion ceiling, so body rewriters (guardrail mask,
// context-assembler expansion) grow/shrink the reservation instead of silently
// widening memory beyond what admission approved. Sequential middleware chain:
// no locking needed.
type bodyReservation struct {
	budget   *InflightBodyBudget
	maxSize  int64
	held     int64
	prealloc int64 // budget pre-acquired for an upcoming rewrite (PreallocBodyGrowth)
}

func (r *bodyReservation) releaseAll() {
	if r == nil {
		return
	}
	r.budget.Release(r.held)
	r.held = 0
}

// readBodyBlock is the granularity at which unknown-length bodies reserve
// budget: each block is acquired BEFORE the buffer grows past it, so a flood
// of chunked requests can never allocate past the cap and only ever
// over-reserves one block per request (512KiB).
const readBodyBlock = 512 << 10

func budgetExhausted(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{"type": "server_error", "message": "server at capacity (inflight body budget exhausted)"},
	})
	c.Abort()
}

// ReadBodyWithBudget is ReadBody plus an in-flight body-bytes budget with
// admission BEFORE the expensive buffering:
//
//   - known Content-Length > maxSize → 413 immediately, reserving NOTHING;
//   - known Content-Length ≤ maxSize → reserved up front, corrected to the
//     actual buffered size once read;
//   - unknown length (chunked / HTTP2) → ENTRY: one slot from a gate sized
//     so the WORST collective read footprint fits the budget, plus one
//     scratch block, both via bounded queueing; GROWTH: the reservation
//     always covers scratch + block chain + transient assembled copy
//     (2T+block), bodies over the per-request ceiling get a deterministic
//     413 (they can never fit the budget), growth within the ceiling can
//     only queue behind holders that never wait themselves (known-CL,
//     multipart, rewrites) — no hold-and-wait, hence no tiling livelock;
//   - every exit path (success, read error, client cancel, over-limit,
//     budget exhaustion) releases exactly what it holds; the success path
//     holds the ACTUAL size until the whole chain (incl. SSE) completes;
//   - multipart upload routes participate in the budget with a conservative
//     reservation (their handlers DO buffer in-process via ParseMultipartForm),
//     released at chain end. budget == nil (or limit <= 0) behaves exactly
//     like ReadBody.
func ReadBodyWithBudget(maxSize int64, budget *InflightBodyBudget, multipartReserve int64) gin.HandlerFunc {
	if multipartReserve <= 0 {
		multipartReserve = 32 << 20
	}
	if budget != nil && budget.limit > 0 {
		// Register this route's unknown-read ceiling at construction so the
		// slot gate (built lazily at first request, after ALL routes are
		// wired) is sized from the worst footprint across routes.
		budget.registerUnknownCeiling(min64(maxSize, (budget.limit-readBodyBlock)/2))
	}
	return func(c *gin.Context) {
		multipart := isFileUploadPath(c.Request.URL.Path) &&
			strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data")

		if multipart {
			// Multipart bodies are not copied into BodyKey (buffering here
			// would truncate/corrupt the framing), but the HANDLER buffers
			// them in-process: FormFile → ParseMultipartForm keeps up to
			// ~32MB of the form in memory (plus file-part copies) before the
			// rest spills to temp files. They therefore participate in the
			// budget with a conservative reservation: Content-Length when
			// known (capped), else the configured reserve. NOT exempt.
			rsv := multipartReserve
			if cl := c.Request.ContentLength; cl > 0 && cl < rsv {
				rsv = cl
			}
			if budget != nil && budget.limit > 0 {
				if !budget.Acquire(rsv) {
					budgetExhausted(c)
					return
				}
			}
			res := &bodyReservation{budget: budget, maxSize: maxSize, held: rsv}
			c.Set(bodyReservationKey, res)
			defer res.releaseAll()
			c.Next()
			return
		}

		// Oversized known-length bodies are rejected BEFORE any reservation:
		// a 40MB Content-Length must neither consume budget nor be read.
		if cl := c.Request.ContentLength; cl > maxSize {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}

		if budget != nil && budget.limit > 0 {
			if cl := c.Request.ContentLength; cl > 0 {
				if !budget.Acquire(cl) {
					budgetExhausted(c)
					return
				}
			}
		}
		res := &bodyReservation{budget: budget, maxSize: maxSize,
			held: max64(0, min64(c.Request.ContentLength, maxSize))}
		c.Set(bodyReservationKey, res)
		defer res.releaseAll()

		var bodyBytes []byte
		if c.Request.ContentLength > 0 {
			// Length known and budget-reserved: a single bounded ReadAll.
			b, err := io.ReadAll(io.LimitReader(c.Request.Body, maxSize+1))
			c.Request.Body.Close()
			if err != nil {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			if int64(len(b)) > maxSize {
				c.AbortWithStatus(http.StatusRequestEntityTooLarge)
				return
			}
			bodyBytes = b
		} else {
			// Unknown length (chunked / HTTP2). STRUCTURAL no-hold-and-wait
			// admission (partial-reservation tiling livelock fix):
			//
			//   * Ceiling C = min(route maxSize, (limit−block)/2) is the
			//     largest body ONE unknown read may buffer. The budget covers
			//     the full peak footprint — scratch block + block chain (T) +
			//     the transient assembled copy (T) ⇒ 2T+block — so a body over
			//     C can never be served under this budget: deterministic 413,
			//     never a wait. C ≤ maxSize also subsumes the route cap.
			//   * Entry takes ONE slot from a gate sized K = limit / (2C_worst
			//     + block) ≥ 1, so at most K unknown reads are in their read
			//     phase at once and K × worst footprint ≤ limit: an admitted
			//     reader's growth can never be blocked by ANOTHER unknown
			//     reader. That removes the tiling livelock (six 512KiB
			//     first-blocks tiling a 3MiB budget, everyone holding while
			//     waiting) BY CONSTRUCTION, not by timeout.
			//   * The slot and the entry scratch block use bounded queueing;
			//     they can only ever wait on holders that NEVER wait
			//     themselves (known-CL, multipart and rewrites acquire
			//     immediately-or-fail), so the wait graph is acyclic.
			//   * Small requests stay usable when limit < maxSize (the naive
			//     slots=floor(limit/maxSize) would round to zero and starve
			//     everything): C shrinks to fit, K floors at 1, bodies ≤ C
			//     still complete — only bodies that can never fit are 413ed.
			ceil := min64(maxSize, (budget.limit-readBodyBlock)/2)
			if ceil <= 0 {
				// Budget smaller than one scratch block + copy headroom: no
				// unknown read can ever fit — reject deterministically.
				c.Request.Body.Close()
				budgetExhausted(c)
				return
			}
			slots := budget.unknownSlots()
			select {
			case slots <- struct{}{}:
			case <-time.After(admissionWait):
				c.Request.Body.Close()
				budgetExhausted(c)
				return
			}
			readUnknown := func() ([]byte, bool) {
				// Slot is held for the READ phase only; downstream handler /
				// SSE lifetime holds just the settled body reservation.
				defer func() { <-slots }()
				if !budget.AcquireWait(readBodyBlock, admissionWait) {
					budgetExhausted(c)
					return nil, false
				}
				res.held += readBodyBlock
				reserved := int64(readBodyBlock) // scratch buffer
				tmp := make([]byte, readBodyBlock)
				var blocks [][]byte // block chain: no grown-buffer realloc (old+new coexist in the naive grow)
				var total int64
				for {
					n, rerr := c.Request.Body.Read(tmp)
					if n > 0 {
						if total+int64(n) > ceil {
							c.Request.Body.Close()
							c.AbortWithStatus(http.StatusRequestEntityTooLarge)
							return nil, false
						}
						// Reserve BEFORE any allocation, covering the chain
						// (total+n), the transient assembled copy (total+n)
						// and the scratch block — rounded up to whole blocks.
						need := 2*(total+int64(n)) + readBodyBlock
						for need > reserved {
							delta := ((need - reserved + readBodyBlock - 1) / readBodyBlock) * readBodyBlock
							if !budget.AcquireWait(delta, admissionWait) {
								budgetExhausted(c)
								return nil, false
							}
							reserved += delta
							res.held += delta
						}
						blk := make([]byte, n)
						copy(blk, tmp[:n])
						blocks = append(blocks, blk)
						total += int64(n)
					}
					if rerr == io.EOF {
						break
					}
					// Client cancel surfaces here as a read error too — the
					// deferred releaseAll returns every held block.
					if rerr != nil {
						c.Request.Body.Close()
						c.AbortWithStatus(http.StatusBadRequest)
						return nil, false
					}
				}
				c.Request.Body.Close()
				// Single final assembly: the transient 2x (chain + assembled
				// body) was covered by the reservation above; the chain is
				// garbage afterwards (heap-peak regression test documents the
				// bound). Settle at the ACTUAL size for the handler lifetime.
				body := make([]byte, total)
				var off int64
				for _, b := range blocks {
					copy(body[off:], b)
					off += int64(len(b))
				}
				budget.Release(reserved - total)
				res.held -= reserved - total
				return body, true
			}
			var ok bool
			bodyBytes, ok = readUnknown()
			if !ok {
				return
			}
		}

		// Correct the reservation to the ACTUAL buffered size (top-up for
		// understated Content-Length; refund for over-statement; refund the
		// unused tail of the last block on unknown-length reads).
		actual := int64(len(bodyBytes))
		switch {
		case actual > res.held:
			if budget != nil && budget.limit > 0 && !budget.Acquire(actual-res.held) {
				budgetExhausted(c)
				return
			}
			res.held = actual
		case actual < res.held:
			budget.Release(res.held - actual)
			res.held = actual
		}
		c.Set(BodyKey, bodyBytes)
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		c.Next()
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func GetBodyBytes(c *gin.Context) []byte {
	val, _ := c.Get(BodyKey)
	if b, ok := val.([]byte); ok {
		return b
	}
	return nil
}

// PreallocBodyGrowth reserves budget for an UPCOMING body rewrite BEFORE the
// rewrite's big allocation happens (e.g. the context assembler's json.Marshal
// of the expanded request). Acceptance round 2: a per-request ceiling alone
// does not stop N concurrent expansions from collectively OOMing — the delta
// must come out of the GLOBAL budget before the Marshal. Returns false when
// the budget cannot cover it (caller aborts). SetBodyBytes consumes the
// pre-allocation when the rewrite lands.
func PreallocBodyGrowth(c *gin.Context, estBytes int64) bool {
	if res, ok := c.Get(bodyReservationKey); ok {
		if r, ok := res.(*bodyReservation); ok && estBytes > 0 {
			if !r.budget.Acquire(estBytes) {
				return false
			}
			r.held += estBytes
			r.prealloc += estBytes
		}
	}
	return true
}

// SetBodyBytes replaces the buffered request body. Downstream middleware and
// handlers read the buffer via GetBodyBytes in preference to c.Request.Body,
// so ANY rewriter of the body (guardrail masking, context-assembler template
// expansion) must update both the reader and the buffer. The rewrite stays
// inside the memory envelope admission approved: a larger replacement first
// checks the per-request expansion ceiling (maxSize) and acquires the DELTA
// from the in-flight budget. Returns false when the new body exceeds the
// ceiling or the budget — the caller must abort the request; the buffer is
// left untouched in that case.
func SetBodyBytes(c *gin.Context, body []byte) bool {
	if res, ok := c.Get(bodyReservationKey); ok {
		if r, ok := res.(*bodyReservation); ok {
			delta := int64(len(body)) - int64(len(GetBodyBytes(c)))
			if int64(len(body)) > r.maxSize {
				// Consume any pre-allocation so it is not stranded.
				r.budget.Release(r.prealloc)
				r.held -= r.prealloc
				r.prealloc = 0
				return false
			}
			// Consume the pre-allocation first (PreallocBodyGrowth); only the
			// remainder needs a fresh acquire.
			if delta > 0 {
				pre := r.prealloc
				if pre > delta {
					pre = delta
				}
				r.prealloc -= pre
				delta -= pre
				if delta > 0 {
					if !r.budget.Acquire(delta) {
						return false
					}
				}
			} else {
				// Rewrite did not grow: refund the whole pre-allocation.
				if r.prealloc > 0 {
					r.budget.Release(r.prealloc)
					r.held -= r.prealloc
					r.prealloc = 0
				}
				if delta < 0 {
					r.budget.Release(-delta)
				}
			}
			r.held += delta
		}
	}
	c.Set(BodyKey, body)
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	return true
}

// isFileUploadPath reports whether the route accepts multipart file uploads and
// therefore must skip body buffering (which would truncate/corrupt large files).
// Currently: audio transcription/translation and image edits.
func isFileUploadPath(path string) bool {
	return strings.HasPrefix(path, "/v1/audio/") ||
		strings.HasPrefix(path, "/v1/images/")
}

// budgetExhaustedAbort is the assembler's budget-exhausted response (kept here
// so both admission points share one wire shape).
func budgetExhaustedAbort(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{"type": "server_error", "message": "server at capacity (inflight body budget exhausted)"},
	})
	c.Abort()
}
