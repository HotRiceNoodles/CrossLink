package middleware

// r63 probe defect regression (chain-order half): the Cache middleware must
// sit INNERMOST — after BudgetCheck and ReportBudgetUsage — so a HIT still
// passes budget admission, its reservation is settled from the restored
// token/price context, and the injected biller writes one durable row per
// served HIT. Pre-fix this test fails exactly like the live probe: the 2nd
// identical request returns 200 in ~1ms with NO settlement and NO biller
// call, while an over-budget key keeps being served from the cache.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/glebarez/sqlite"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

type nopAlertSvc struct{}

func (nopAlertSvc) CheckAndAlert(ctx context.Context, scope, targetID, periodType string, spent, budget float64) {}

type hitBillRecorder struct {
	mu     sync.Mutex
	calls  int
	lastMu sync.Mutex
	lastIn, lastOut float64
}

func (h *hitBillRecorder) bill(c *gin.Context, cached *service.CacheEntry) {
	h.mu.Lock()
	h.calls++
	h.mu.Unlock()
	h.lastMu.Lock()
	h.lastIn, h.lastOut = cached.InputPrice, cached.OutputPrice
	h.lastMu.Unlock()
}

// buildChain wires BudgetCheck → ReportBudgetUsage → Cache(innermost) over
// one miniredis, mirroring app.go's gateway order for the billing-relevant
// middlewares (rate/TPM omitted — not under test here).
func buildChain(t *testing.T, apiKey *model.APIKey) (*gin.Engine, *redis.Client, *hitBillRecorder) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	budgetSvc := service.NewBudgetService(rdb)

	// GetModelCacheConfig falls through to the DB for unknown models — hand
	// it an empty in-memory sqlite so the miss path returns (0,false) and
	// the endpoint default TTL applies.
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.ProviderModel{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	cacheSvc := service.NewCacheService(rdb, gdb, config.CacheConfig{
		Enabled:    true,
		DefaultTTL: 5 * time.Minute,
		MaxBodySize: 1 << 20,
	})
	cp, _ := crypto.NewProvider("standard")
	rec := &hitBillRecorder{}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	var reqSeq int64
	r.Use(func(c *gin.Context) {
		// Mirror the real RequestID middleware: a FRESH internal id per
		// request. The F1′ settle guard keys on it — a constant id (as in an
		// early draft of this test) makes the 2nd settle a duplicate no-op
		// and the test proves nothing.
		reqSeq++
		c.Set("api_key", apiKey)
		c.Set("api_key_id", apiKey.ID) // mirrors the real Auth middleware (buildCacheKey reads it)
		c.Set("request_id", fmt.Sprintf("11111111-2222-3333-4444-%012d", reqSeq))
		c.Next()
	})
	r.Use(BudgetCheck(budgetSvc, nil, nil))
	r.Use(ReportBudgetUsage(budgetSvc, nopAlertSvc{}, nil, nil))
	r.Use(Cache(cacheSvc, cp, nil, rec.bill))
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		// Stand-in for the non-stream handler: publishes the billing context
		// the real handler sets, and a cacheable 200 body with usage.
		c.Set("input_tokens", 12)
		c.Set("output_tokens", 6)
		c.Set("input_price", 3.0)
		c.Set("output_price", 12.0)
		c.Set("currency", "CNY")
		c.Set("usage_logged", true)
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":12,"completion_tokens":6}}`))
	})
	return r, rdb, rec
}

func TestCacheHit_IsBilledSettledAndAdmitted(t *testing.T) {
	apiKey := &model.APIKey{ID: 990655, MaxBudget: 10, BudgetPeriod: "monthly"}
	r, rdb, rec := buildChain(t, apiKey)

	body := `{"model":"mock-r63","stream":false,"messages":[{"role":"user","content":"p"}]}`
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	w1 := post()
	if w1.Code != 200 || w1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first request: status=%d X-Cache=%q, want 200/MISS", w1.Code, w1.Header().Get("X-Cache"))
	}
	w2 := post()
	if w2.Code != 200 || w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second identical request: status=%d X-Cache=%q, want 200/HIT", w2.Code, w2.Header().Get("X-Cache"))
	}

	// The HIT must have gone through the biller exactly once.
	rec.mu.Lock()
	calls := rec.calls
	rec.mu.Unlock()
	if calls != 1 {
		t.Fatalf("cache-hit biller calls = %d, want 1 — the HIT was served without billing", calls)
	}

	// Budget settled TWICE (the MISS settle + the HIT settle): cost per
	// request = 12*3/1000 + 6*12/1000 = 0.108.
	pk := service.PeriodKey("monthly")
	raw := rdb.Get(context.Background(), "budget:key:990655:"+pk).Val()
	got, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("budget counter %q not numeric: %v", raw, err)
	}
	if diff := got - 0.216; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("key budget settled = %v, want 0.216 (two billed servings of 0.108)", got)
	}
	resv := rdb.Get(context.Background(), "budgetresv:key:990655:"+pk).Val()
	if resv != "0" && resv != "" {
		t.Fatalf("residual reservation = %q, want 0/absent after settle", resv)
	}
}

// An over-budget key must NOT be served from the cache for free: the HIT
// path passes BudgetCheck admission, which rejects before the cache is read.
func TestCacheHit_OverBudgetKey_IsRejected(t *testing.T) {
	apiKey := &model.APIKey{ID: 990656, MaxBudget: 0.1, BudgetPeriod: "monthly"}
	r, rdb, rec := buildChain(t, apiKey)
	pk := service.PeriodKey("monthly")

	body := `{"model":"mock-r63","stream":false,"messages":[{"role":"user","content":"p"}]}`
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := post(); w.Code != 200 {
		t.Fatalf("populating request status=%d, want 200", w.Code)
	}
	// Over the limit now: even a cached body must be rejected.
	rdb.Set(context.Background(), "budget:key:990656:"+pk, "0.5", time.Hour)
	w2 := post()
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("over-budget cache HIT status=%d, want 429 (cache must not bypass budget enforcement)", w2.Code)
	}
	if calls := func() int { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.calls }(); calls != 0 {
		t.Fatalf("biller ran %d times for a rejected request, want 0", calls)
	}
}

// Requests carrying X-Idempotency-Key belong to the handler's idempotency
// layer (bill exactly once per event). The semantic cache must neither
// serve nor store them — otherwise a replay of the same body would be
// billed again as a cache HIT on top of the idem layer's bill-once.
func TestCacheSkips_IdempotencyKeyRequests(t *testing.T) {
	apiKey := &model.APIKey{ID: 990657, MaxBudget: 10, BudgetPeriod: "monthly"}
	r, _, rec := buildChain(t, apiKey)

	body := `{"model":"mock-r63","stream":false,"messages":[{"role":"user","content":"p"}]}`
	hits := 0
	// A path of its own so the shared stand-in route from buildChain does not
	// also fire; the cache middleware is group-wide.
	r.POST("/v1/test/idem", func(c *gin.Context) { hits++; c.String(200, "upstream response") })

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/test/idem", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Idempotency-Key", "replay-once-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < 2; i++ {
		w := post()
		if w.Code != 200 {
			t.Fatalf("idem request %d status=%d, want 200", i+1, w.Code)
		}
		if w.Header().Get("X-Cache") != "" {
			t.Fatalf("idem request %d got X-Cache=%q — the semantic cache must not touch idempotent requests", i+1, w.Header().Get("X-Cache"))
		}
	}
	if hits != 2 {
		t.Fatalf("handler executions = %d, want 2 (each idem request runs the real pipeline)", hits)
	}
	if calls := func() int { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.calls }(); calls != 0 {
		t.Fatalf("cache-hit biller ran %d times, want 0", calls)
	}
}

// writeCacheEntryAt stores a raw CacheEntry at exactly the key the Cache
// middleware computes for (path, body, api_key), letting tests manufacture
// LEGACY entries (pre-price-snapshot, Priced absent) the way a pre-upgrade
// deployment would have left them in Redis.
func writeCacheEntryAt(t *testing.T, rdb *redis.Client, cp crypto.CryptoProvider, apiKey *model.APIKey, path, body string, entry service.CacheEntry) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request, _ = http.NewRequest(http.MethodPost, path, nil)
	c.Set("api_key_id", apiKey.ID)
	key := buildCacheKey(path, []byte(body), c, cp)
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	if err := rdb.Set(context.Background(), key, data, time.Minute).Err(); err != nil {
		t.Fatalf("seed entry: %v", err)
	}
}

// r65 review: an unpriced legacy entry must NOT be served (the old
// "declared zero-cost transition" is rejected) — it bypasses upstream,
// re-bills at today's prices, and the entry is overwritten priced.
func TestCacheBypass_UnpricedLegacyEntry(t *testing.T) {
	apiKey := &model.APIKey{ID: 990658, MaxBudget: 10, BudgetPeriod: "monthly"}
	r, rdb, rec := buildChain(t, apiKey)
	cp, _ := crypto.NewProvider("standard")

	body := `{"model":"mock-r64","stream":false,"messages":[{"role":"user","content":"legacy"}]}`
	hits := 0
	r.POST("/v1/test/legacy", func(c *gin.Context) {
		hits++
		c.Set("input_tokens", 10)
		c.Set("output_tokens", 5)
		c.Set("input_price", 3.0)
		c.Set("output_price", 12.0)
		c.Set("currency", "CNY")
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"fresh"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/test/legacy", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// Seed a LEGACY entry: tokens present, no price snapshot, no Priced flag.
	writeCacheEntryAt(t, rdb, cp, apiKey, "/v1/test/legacy", body, service.CacheEntry{
		Model: "mock-r64", Endpoint: "/v1/test/legacy",
		Body:        json.RawMessage(`{"choices":[{"message":{"content":"stale legacy"}}]}`),
		InputTokens: 10, OutputTokens: 5,
	})

	w1 := post()
	if w1.Code != 200 || w1.Header().Get("X-Cache") != "BYPASS-UNPRICED" {
		t.Fatalf("legacy entry request: status=%d X-Cache=%q, want 200/BYPASS-UNPRICED", w1.Code, w1.Header().Get("X-Cache"))
	}
	if hits != 1 {
		t.Fatalf("handler executions = %d, want 1 (unpriced entry must re-execute upstream)", hits)
	}
	if !strings.Contains(w1.Body.String(), "fresh") {
		t.Fatal("served the stale legacy body — bypass must regenerate, not serve")
	}
	if calls := func() int { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.calls }(); calls != 0 {
		t.Fatalf("biller ran %d times on a bypassed legacy entry, want 0", calls)
	}

	// The re-execution overwrote the entry with a PRICED snapshot: the next
	// identical request is a normal billed HIT.
	w2 := post()
	if w2.Code != 200 || w2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("post-overwrite request: status=%d X-Cache=%q, want 200/HIT", w2.Code, w2.Header().Get("X-Cache"))
	}
	if hits != 1 {
		t.Fatalf("handler executions = %d, want 1 (second request served from cache)", hits)
	}
	if calls := func() int { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.calls }(); calls != 1 {
		t.Fatalf("biller ran %d times on the priced HIT, want 1", calls)
	}
}

// A genuine zero-price model (Priced=true, prices 0) is billable at 0 and
// must be served from cache — the bypass is keyed on the MISSING snapshot,
// not on the zero value.
func TestCacheServes_PricedZeroPriceEntry(t *testing.T) {
	apiKey := &model.APIKey{ID: 990659, MaxBudget: 10, BudgetPeriod: "monthly"}
	r, rdb, rec := buildChain(t, apiKey)
	cp, _ := crypto.NewProvider("standard")

	body := `{"model":"free-model","stream":false,"messages":[{"role":"user","content":"z"}]}`
	hits := 0
	r.POST("/v1/test/free", func(c *gin.Context) {
		hits++
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	})
	writeCacheEntryAt(t, rdb, cp, apiKey, "/v1/test/free", body, service.CacheEntry{
		Model: "free-model", Endpoint: "/v1/test/free",
		Body:        json.RawMessage(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`),
		InputTokens: 7, OutputTokens: 3,
		InputPrice: 0, OutputPrice: 0, Currency: "CNY",
		Priced: true,
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/test/free", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("zero-price entry: status=%d X-Cache=%q, want 200/HIT", w.Code, w.Header().Get("X-Cache"))
	}
	if hits != 0 {
		t.Fatalf("handler ran %d times, want 0 (priced entry serves from cache)", hits)
	}
	if calls := func() int { rec.mu.Lock(); defer rec.mu.Unlock(); return rec.calls }(); calls != 1 {
		t.Fatalf("biller ran %d times, want 1 (zero-price serving still writes its row)", calls)
	}
}

// Price-change audit (r65 review): a cached entry bills at the price
// SNAPSHOT taken when it was populated — a provider price change does not
// retroactively re-price existing entries; after the entry is gone (TTL or
// ops flush) the next MISS re-executes and re-snapshots at the NEW price.
// Operational boundary: a price/currency change takes effect for cached
// bodies after cache TTL, or immediately via a cl:cache:* flush.
func TestCachePricingSnapshot_PriceChangeAudit(t *testing.T) {
	apiKey := &model.APIKey{ID: 990660, MaxBudget: 100, BudgetPeriod: "monthly"}
	r, rdb, rec := buildChain(t, apiKey)

	body := `{"model":"mock-r64","stream":false,"messages":[{"role":"user","content":"price-audit"}]}`
	curIn, curOut := 3.0, 12.0
	r.POST("/v1/test/price", func(c *gin.Context) {
		c.Set("input_tokens", 10)
		c.Set("output_tokens", 5)
		c.Set("input_price", curIn)
		c.Set("output_price", curOut)
		c.Set("currency", "CNY")
		c.Data(http.StatusOK, "application/json", []byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	})
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/test/price", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	post() // MISS @ 3/12 → entry snapshot 3/12
	rec.lastMu.Lock()
	rec.lastIn, rec.lastOut = 0, 0
	rec.lastMu.Unlock()

	post() // HIT @ snapshot 3/12
	rec.lastMu.Lock()
	in, out := rec.lastIn, rec.lastOut
	rec.lastMu.Unlock()
	if in != 3 || out != 12 {
		t.Fatalf("HIT billed at %v/%v, want snapshot 3/12 (price changes must not re-price live entries)", in, out)
	}

	// Provider price change (route now serves at 6/24) + ops cache flush:
	curIn, curOut = 6, 24
	if err := rdb.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	post() // MISS @ new 6/24
	rec.lastMu.Lock()
	rec.lastMu.Unlock()
	post() // HIT @ new snapshot 6/24
	rec.lastMu.Lock()
	in, out = rec.lastIn, rec.lastOut
	rec.lastMu.Unlock()
	if in != 6 || out != 24 {
		t.Fatalf("post-flush HIT billed at %v/%v, want new snapshot 6/24", in, out)
	}
}
