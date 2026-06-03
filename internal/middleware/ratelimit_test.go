package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers ---

func setupTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb, mr
}

func newTestTeamCache(teams ...*model.Team) *TeamCache {
	tc := &TeamCache{
		items: make(map[int64]*teamCacheEntry),
		ttl:   30 * time.Second,
	}
	for _, team := range teams {
		tc.items[team.ID] = &teamCacheEntry{team: team, expiresAt: time.Now().Add(30 * time.Second)}
	}
	return tc
}

func newTestOrgCache(orgs ...*model.Organization) *OrgCache {
	oc := &OrgCache{
		items: make(map[int64]*orgCacheEntry),
		ttl:   30 * time.Second,
	}
	for _, org := range orgs {
		oc.items[org.ID] = &orgCacheEntry{org: org, expiresAt: time.Now().Add(30 * time.Second)}
	}
	return oc
}

// setupRLRouter creates a test gin.Engine with the given middleware and a dummy handler.
func setupRLRouter(mw gin.HandlerFunc, apiKey *model.APIKey, keyID int64, extraSetup func(*gin.Context)) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if keyID > 0 {
			c.Set("api_key_id", keyID)
		}
		if apiKey != nil {
			c.Set("api_key", apiKey)
			if apiKey.TeamID != nil {
				c.Set("team_id", *apiKey.TeamID)
			}
		}
		if extraSetup != nil {
			extraSetup(c)
		}
		c.Next()
	})
	r.Use(mw)
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}

func makeRequest(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w, req)
	return w
}

// --- RPM Tests ---

func TestRateLimit_UnderLimit_Passes(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1, RPMLimit: 5}
	r := setupRLRouter(RateLimit(rdb, 60, nil, nil), apiKey, 1, nil)

	for i := 0; i < 5; i++ {
		w := makeRequest(r)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code, "request %d should pass", i+1)
	}
}

func TestRateLimit_OverLimit_Rejects(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1, RPMLimit: 3}
	r := setupRLRouter(RateLimit(rdb, 60, nil, nil), apiKey, 1, nil)

	for i := 0; i < 3; i++ {
		w := makeRequest(r)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code)
	}

	// 4th request should be rejected
	w := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestRateLimit_ZeroRPM_Skips(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	r := setupRLRouter(RateLimit(rdb, 0, nil, nil), nil, 1, nil)

	w := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w.Code)
}

func TestRateLimit_TeamLevelLimit(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	teamID := int64(10)
	apiKey := &model.APIKey{ID: 1, TeamID: &teamID}
	teamCache := newTestTeamCache(&model.Team{ID: 10, RPMLimit: 2})
	r := setupRLRouter(RateLimit(rdb, 100, teamCache, nil), apiKey, 1, nil)

	// First 2 pass
	w1 := makeRequest(r)
	w2 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)
	assert.NotEqual(t, http.StatusTooManyRequests, w2.Code)

	// 3rd rejected at team level
	w3 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w3.Code)
}

// --- TPM Tests ---

func TestTPMLimit_UnderLimit_Passes(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	r := setupRLRouter(TPMLimit(rdb, 10000, nil, nil, 2000, false), apiKey, 1, nil)

	for i := 0; i < 5; i++ {
		w := makeRequest(r)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code, "request %d should pass", i+1)
	}
}

func TestTPMLimit_OverLimit_Rejects(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	// TPM=5000, reservation=2000 → at most 2 requests (2*2000=4000 ≤ 5000)
	r := setupRLRouter(TPMLimit(rdb, 5000, nil, nil, 2000, false), apiKey, 1, nil)

	for i := 0; i < 2; i++ {
		w := makeRequest(r)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code)
	}

	// 3rd should be rejected (4000 + 2000 = 6000 > 5000)
	w := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestTPMLimit_ZeroTPM_Skips(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	r := setupRLRouter(TPMLimit(rdb, 0, nil, nil, 2000, false), nil, 1, nil)

	w := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w.Code)
}

func TestTPMLimit_PerKeyOverride(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1, TPMLimit: 3000}
	// Global TPM=100000 but key override=3000, reservation=2000 → 1 request allowed
	r := setupRLRouter(TPMLimit(rdb, 100000, nil, nil, 2000, false), apiKey, 1, nil)

	// First passes
	w1 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)

	// Second rejected (2000 + 2000 = 4000 > 3000)
	w2 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestTPMLimit_TeamLevelLimit(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	teamID := int64(10)
	apiKey := &model.APIKey{ID: 1, TeamID: &teamID}
	teamCache := newTestTeamCache(&model.Team{ID: 10, TPMLimit: 3000})
	r := setupRLRouter(TPMLimit(rdb, 100000, teamCache, nil, 2000, false), apiKey, 1, nil)

	// First passes (key: 2000, team: 2000)
	w1 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)

	// Second rejected at team level (team: 2000 + 2000 = 4000 > 3000)
	w2 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestTPMLimit_OrgLevelLimit(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	teamID := int64(10)
	orgID := int64(20)
	apiKey := &model.APIKey{ID: 1, TeamID: &teamID}
	teamCache := newTestTeamCache(&model.Team{ID: 10, TPMLimit: 0})
	orgCache := newTestOrgCache(&model.Organization{ID: 20, TPMLimit: 2000})

	r := setupRLRouter(TPMLimit(rdb, 100000, teamCache, orgCache, 2000, false), apiKey, 1, func(c *gin.Context) {
		c.Set("org_id", orgID)
	})

	// First passes (org: 2000 ≤ 2000)
	w1 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)

	// Second rejected at org level (org: 2000 + 2000 = 4000 > 2000)
	w2 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)
}

func TestTPMLimit_RejectedRequestRefundsKeyReservation(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	teamID := int64(10)
	apiKey := &model.APIKey{ID: 1, TeamID: &teamID}
	// Key TPM=100000, Team TPM=2000, reservation=2000
	teamCache := newTestTeamCache(&model.Team{ID: 10, TPMLimit: 2000})
	r := setupRLRouter(TPMLimit(rdb, 100000, teamCache, nil, 2000, false), apiKey, 1, nil)

	// First passes (key reserves 2000, team reserves 2000 ≤ 2000)
	w1 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)

	// Second rejected by team (key reserves 2000, team tries 2000 more → 4000 > 2000 → rollback)
	w2 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)

	// Key-level counter should be refunded: 2000 (first) only
	mr.CheckGet(t, "tpm:key:1", "2000")
}

// --- Concurrent Safety Test (core P0 verification) ---

func TestTPMLimit_ConcurrentRequests_NoOvershoot(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	// TPM=10000, reservation=2000 → max 5 concurrent requests allowed (5*2000=10000)
	mw := TPMLimit(rdb, 10000, nil, nil, 2000, false)

	var allowed, rejected int64
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := setupRLRouter(mw, apiKey, 1, nil)
			w := makeRequest(r)
			if w.Code == http.StatusTooManyRequests {
				atomic.AddInt64(&rejected, 1)
			} else {
				atomic.AddInt64(&allowed, 1)
			}
		}()
	}
	wg.Wait()

	t.Logf("allowed=%d, rejected=%d", allowed, rejected)
	assert.Equal(t, int64(5), allowed, "exactly 5 requests should be allowed (5*2000=10000)")
	assert.Equal(t, int64(15), rejected, "15 requests should be rejected")

	// Verify Redis counter is exactly 10000 (5 reservations of 2000 each)
	mr.CheckGet(t, "tpm:key:1", "10000")
}

// --- ReportTokens Tests ---

func TestReportTokens_AdjustsReservation(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	// Pre-set a reservation in Redis
	rdb.Set(context.Background(), "tpm:key:1", "2000", time.Minute).Err()

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/test", nil)
	c.Set("tpm_key", "tpm:key:1")
	c.Set("tpm_reservations", &tpmReservations{Key: 2000})
	c.Set("input_tokens", 1200)
	c.Set("output_tokens", 300)

	handler := ReportTokens(rdb, nil)
	handler(c)

	// Actual: 1500. Delta: 1500 - 2000 = -500. Counter: 2000 + (-500) = 1500
	mr.CheckGet(t, "tpm:key:1", "1500")
}

func TestReportTokens_NoTokens_NoIncrement(t *testing.T) {
	rdb, mr := setupTestRedis(t)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/test", nil)
	c.Set("tpm_key", "tpm:key:1")

	handler := ReportTokens(rdb, nil)
	handler(c)

	// No tokens → key should not exist
	assert.False(t, mr.Exists("tpm:key:1"))
}

func TestReportTokens_ZeroReservation_ReportsFullTotal(t *testing.T) {
	rdb, mr := setupTestRedis(t)

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/test", nil)
	c.Set("tpm_key", "tpm:key:1")
	// No tpm_reservations set → all reservations are 0 → delta = total - 0 = total
	c.Set("input_tokens", 500)
	c.Set("output_tokens", 300)

	handler := ReportTokens(rdb, nil)
	handler(c)

	// total = 800, no reservation → delta = 800
	mr.CheckGet(t, "tpm:key:1", "800")
}

func TestReportTokens_TeamAndOrgCounters(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	teamID := int64(10)
	orgID := int64(20)
	apiKey := &model.APIKey{ID: 1, TeamID: &teamID}
	orgCache := newTestOrgCache(&model.Organization{ID: 20, TPMLimit: 0})

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/test", nil)
	c.Set("tpm_key", "tpm:key:1")
	c.Set("tpm_reservations", &tpmReservations{Key: 2000, Team: 2000, Org: 2000})
	c.Set("input_tokens", 1500)
	c.Set("output_tokens", 500)
	c.Set("api_key", apiKey)
	c.Set("team_id", teamID)
	c.Set("org_id", orgID)

	handler := ReportTokens(rdb, orgCache)
	handler(c)

	// All levels: delta = 2000 - 2000 = 0 → no change to keys that already exist
	// But since these keys don't exist yet, delta=0 means no INCRBY call
	assert.False(t, mr.Exists("tpm:key:1"))
	assert.False(t, mr.Exists("tpm:team:10"))
	assert.False(t, mr.Exists("tpm:org:20"))
}

// --- Full Cycle Test ---

func TestTPMLimit_FullCycle(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}

	gin.SetMode(gin.TestMode)

	tpmMW := TPMLimit(rdb, 10000, nil, nil, 2000, false)
	reportMW := ReportTokens(rdb, nil)

	// Set up full middleware chain: TPM check → dummy handler that sets tokens → report
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("api_key_id", int64(1))
		c.Set("api_key", apiKey)
		c.Next()
	})
	r.Use(tpmMW)
	r.Use(func(c *gin.Context) {
		// Simulate handler setting token counts
		c.Set("input_tokens", 3000)
		c.Set("output_tokens", 1000)
		c.Next()
	})
	r.Use(reportMW)
	r.POST("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// Request 1: 4000 actual tokens, 2000 reserved
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w1, req1)
	assert.Equal(t, http.StatusOK, w1.Code)

	// After request 1: counter = 2000(reserve) + (4000-2000)(adjust) = 4000
	mr.CheckGet(t, "tpm:key:1", "4000")

	// Request 2: 4000 actual tokens
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)

	// After request 2: counter = 4000 + 2000(reserve) + (4000-2000)(adjust) = 8000
	mr.CheckGet(t, "tpm:key:1", "8000")

	// Request 3: 2000 more reserved = 10000 ≤ 10000, passes
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w3, req3)
	assert.Equal(t, http.StatusOK, w3.Code)

	// Request 4: 12000 + 2000 > 10000, rejected
	w4 := httptest.NewRecorder()
	req4, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w4, req4)
	assert.Equal(t, http.StatusTooManyRequests, w4.Code)
}

// --- Edge Cases ---

func TestTPMLimit_DefaultReservation(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	// reservationAmount=0 → should use default 2000
	r := setupRLRouter(TPMLimit(rdb, 5000, nil, nil, 0, false), apiKey, 1, nil)

	for i := 0; i < 2; i++ {
		w := makeRequest(r)
		assert.NotEqual(t, http.StatusTooManyRequests, w.Code)
	}

	w := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestTPMLimit_RejectedRequestDoesNotLeakReservation(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	r := setupRLRouter(TPMLimit(rdb, 2000, nil, nil, 2000, false), apiKey, 1, nil)

	// First request allowed (2000 ≤ 2000)
	w1 := makeRequest(r)
	assert.NotEqual(t, http.StatusTooManyRequests, w1.Code)

	// Second request rejected (2000 + 2000 = 4000 > 2000, script DECRBY back)
	w2 := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w2.Code)

	// Counter should be 2000 (first reservation only, second was rolled back)
	mr.CheckGet(t, "tpm:key:1", "2000")
}

func TestTPMLimit_SetsReservationContext(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}

	gin.SetMode(gin.TestMode)
	var capturedRes *tpmReservations

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("api_key_id", int64(1))
		c.Set("api_key", apiKey)
		c.Next()
	})
	r.Use(TPMLimit(rdb, 10000, nil, nil, 2000, false))
	r.POST("/test", func(c *gin.Context) {
		if val, exists := c.Get("tpm_reservations"); exists {
			capturedRes = val.(*tpmReservations)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/test", nil)
	r.ServeHTTP(w, req)

	require.NotNil(t, capturedRes, "tpm_reservations should be set in context")
	assert.Equal(t, 2000, capturedRes.Key, "key reservation should be 2000")
	assert.Equal(t, 0, capturedRes.Team, "team reservation should be 0 (no team)")
	assert.Equal(t, 0, capturedRes.Org, "org reservation should be 0 (no org)")
}

// --- Phase 3: Token Estimation Tests ---

func TestEstimateTokens_ChatMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Hello world, this is a test message for token estimation"}],"max_tokens":500}`)
	c.Set(BodyKey, body)

	estimated := estimateTokens(c)
	assert.Greater(t, estimated, 0, "should estimate tokens from chat messages")
	// Input tokens for "Hello world, this is a test message for token estimation" ≈ 10
	// Plus max_tokens=500 → should be around 510
	assert.GreaterOrEqual(t, estimated, 500, "should include max_tokens")
}

func TestEstimateTokens_CompletionPrompt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"model":"gpt-4","prompt":"Translate the following text to French","max_tokens":256}`)
	c.Set(BodyKey, body)

	estimated := estimateTokens(c)
	assert.Greater(t, estimated, 0, "should estimate tokens from prompt field")
	assert.GreaterOrEqual(t, estimated, 256, "should include max_tokens")
}

func TestEstimateTokens_NoMaxTokens_DefaultsTo1024(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Hello world, this is a longer test message"}]}`)
	c.Set(BodyKey, body)

	estimated := estimateTokens(c)
	assert.GreaterOrEqual(t, estimated, 1024, "should default to 1024 output tokens when max_tokens not set")
}

func TestEstimateTokens_NoBody_ReturnsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	estimated := estimateTokens(c)
	assert.Equal(t, 0, estimated, "should return 0 when no body")
}

func TestEstimateTokens_InvalidJSON_ReturnsZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set(BodyKey, []byte(`not json`))

	estimated := estimateTokens(c)
	assert.Equal(t, 0, estimated, "should return 0 for invalid JSON")
}

// --- Phase 3: Fail-Closed Tests ---

func TestTPMLimit_FailClosed_RedisError(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}

	// Close miniredis to simulate Redis failure
	mr.Close()

	r := setupRLRouter(TPMLimit(rdb, 10000, nil, nil, 2000, true), apiKey, 1, nil)
	w := makeRequest(r)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "should return 503 when fail-closed and Redis is down")
}

func TestTPMLimit_FailOpen_RedisError(t *testing.T) {
	rdb, mr := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}

	// Close miniredis to simulate Redis failure
	mr.Close()

	r := setupRLRouter(TPMLimit(rdb, 10000, nil, nil, 2000, false), apiKey, 1, nil)
	w := makeRequest(r)
	assert.Equal(t, http.StatusOK, w.Code, "should pass through when fail-open (default) and Redis is down")
}

// --- Phase 4: X-RateLimit Headers Tests ---

func TestTPMLimit_SetsRateLimitHeaders(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	r := setupRLRouter(TPMLimit(rdb, 10000, nil, nil, 2000, false), apiKey, 1, nil)

	w := makeRequest(r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "10000", w.Header().Get("X-RateLimit-Limit"), "should set limit header")
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Remaining"), "should set remaining header")
}

func TestTPMLimit_RateLimitHeadersOnRejection(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1}
	r := setupRLRouter(TPMLimit(rdb, 2000, nil, nil, 2000, false), apiKey, 1, nil)

	// First request passes
	makeRequest(r)

	// Second request rejected
	w := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "2000", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("Retry-After"), "should set Retry-After header")
}

func TestRateLimit_SetsHeaders(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1, RPMLimit: 10}
	r := setupRLRouter(RateLimit(rdb, 60, nil, nil), apiKey, 1, nil)

	w := makeRequest(r)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "10", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "9", w.Header().Get("X-RateLimit-Remaining"))
}

func TestRateLimit_HeadersOnRejection(t *testing.T) {
	rdb, _ := setupTestRedis(t)
	apiKey := &model.APIKey{ID: 1, RPMLimit: 2}
	r := setupRLRouter(RateLimit(rdb, 60, nil, nil), apiKey, 1, nil)

	makeRequest(r)
	makeRequest(r)

	w := makeRequest(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "2", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}
