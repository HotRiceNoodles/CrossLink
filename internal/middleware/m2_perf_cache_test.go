package middleware

// M2 sub-batch 2 (review remediation item 3): the REAL middleware.Cache HIT
// path (X-Cache), distinct from idempotent replay. Drives the actual cache
// middleware + Redis-backed CacheService (miniredis) + downstream handler
// with an upstream-call counter. Proves, per scenario, that:
//   - HIT serves the cached body with ZERO downstream/upstream invocations;
//   - every HIT re-runs the CURRENT response policy (behavioural proof:
//     after a policy change rejects the cached body, the request is served
//     BYPASS-GUARDRAIL with a fresh regeneration — the stale entry never
//     reaches the client);
// and measures warm HIT latency plus the regeneration path.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func cachePct(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	return s[int(float64(len(s)-1)*p)]
}

func TestM2PerfRealCacheHitAndPolicyRecheck(t *testing.T) {
	if os.Getenv("M2_PERF") != "1" || testing.Short() {
		t.Skip("set M2_PERF=1 to run the real cache HIT benchmark")
	}
	gin.SetMode(gin.TestMode)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// db: no provider_models table ⇒ GetModelCacheConfig falls back to default TTL
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	for _, stmt := range []string{
		"CREATE TABLE system_settings (key TEXT, value TEXT)",
		"INSERT INTO system_settings VALUES ('guardrails_enabled','true')",
		"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	gs := guardrail.NewGuardrailService(db, nil)
	gs.RefreshSettings(t.Context())

	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatal(err)
	}
	cacheSvc := service.NewCacheService(rdb, db, config.CacheConfig{Enabled: true, DefaultTTL: time.Hour, MaxBodySize: 1 << 20})

	body := `{"model":"cache-bench","messages":[{"role":"user","content":"` + strings.Repeat("summarize the quarterly regional report. ", 24) + `"}]}`
	cachedBody := []byte(`{"id":"cmpl-bench","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + strings.Repeat("regional revenue grew steadily across all markets this quarter. ", 14) + `"}}]}`)

	downstreamCalls := 0
	r := gin.New()
	r.POST("/v1/chat/completions", Cache(cacheSvc, cp, gs, nil), func(c *gin.Context) {
		downstreamCalls++
		c.Data(http.StatusOK, "application/json", cachedBody) // fresh generation (mock upstream ≈0ms)
	})
	do := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}

	t.Logf("== M2 real middleware.Cache bench (X-Cache path; downstream = mock upstream ≈0ms) ==")

	// Phase 1: MISS populates the cache
	w := do()
	if w.Header().Get("X-Cache") != "MISS" && w.Header().Get("X-Cache") != "" {
		t.Logf("phase1 X-Cache header = %q (MISS-or-unset acceptable)", w.Header().Get("X-Cache"))
	}
	if downstreamCalls != 1 {
		t.Fatalf("phase1: expected 1 downstream call, got %d", downstreamCalls)
	}

	// Phase 2: warm HIT latency — downstream must stay at 1 (zero upstream per HIT)
	for i := 0; i < 20; i++ {
		do()
	}
	if downstreamCalls != 1 {
		t.Fatalf("HIT path touched downstream: calls=%d", downstreamCalls)
	}
	var vals []float64
	for i := 0; i < 200; i++ {
		st := time.Now()
		w := do()
		vals = append(vals, float64(time.Since(st).Microseconds())/1000.0)
		if w.Header().Get("X-Cache") != "HIT" {
			t.Fatalf("expected X-Cache HIT, got %q", w.Header().Get("X-Cache"))
		}
	}
	t.Logf("cache_HIT med n=200 P50=%.2fms P95=%.2fms P99=%.2fms downstream_calls=%d (each HIT re-checked by current policy — behavioural proof below)",
		cachePct(vals, .5), cachePct(vals, .95), cachePct(vals, .99), downstreamCalls)

	// Phase 3: policy change → every HIT re-check runs against CURRENT rules.
	// The cached body now contains a blocked marker under the new policy: the
	// middleware must serve BYPASS-GUARDRAIL (regeneration), never the entry.
	if err := db.Exec(`INSERT INTO guardrail_rules VALUES (9,'kw','keyword_filter','response',true,'{"blocklist":["regional revenue grew steadily"]}','high','block')`).Error; err != nil {
		t.Fatal(err)
	}
	gs.InvalidateCache() // production rule writes invalidate the rules TTL cache
	gs.RefreshSettings(t.Context())
	w = do()
	if w.Header().Get("X-Cache") != "BYPASS-GUARDRAIL" {
		t.Fatalf("policy-changed HIT must be BYPASS-GUARDRAIL, got %q body=%.80s", w.Header().Get("X-Cache"), w.Body.String())
	}
	if downstreamCalls != 2 {
		t.Fatalf("policy-changed HIT must regenerate exactly once: calls=%d", downstreamCalls)
	}
	t.Logf("policy_changed_recheck: X-Cache=BYPASS-GUARDRAIL, downstream regenerated once (calls=%d), stale entry never served ✓", downstreamCalls)

	// Phase 4: regeneration path latency under the rejecting policy
	for i := 0; i < 20; i++ {
		do()
	}
	vals = vals[:0]
	for i := 0; i < 100; i++ {
		st := time.Now()
		do()
		vals = append(vals, float64(time.Since(st).Microseconds())/1000.0)
	}
	t.Logf("cache_BYPASS-GUARDRAIL_regenerate med n=100 P50=%.2fms P95=%.2fms (downstream+rewrites; detection rejected the cached entry each time)",
		cachePct(vals, .5), cachePct(vals, .95))
}
