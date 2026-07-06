package middleware

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// waitForStatsKey polls miniredis until the async RoutingStats goroutine has
// written the stats key (or times out, failing the test).
func waitForStatsKey(t *testing.T, mr *miniredis.Miniredis) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if keys := mr.Keys(); len(keys) > 0 {
			return keys[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("stats key never appeared in redis")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func setupRoutingStatsRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return rdb, mr
}

// newStatsTestRouter mounts a fake "handler" that sets the context values the
// real LLM handlers set (model/provider/tokens/prices/org_id), then RoutingStats.
func newStatsTestRouter(rdb *redis.Client, status int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("model", "glm-5.2")
		c.Set("provider", "zhipu")
		c.Set("input_tokens", 100)
		c.Set("output_tokens", 50)
		c.Set("input_price", 0.01)
		c.Set("output_price", 0.02)
		c.Set("org_id", int64(7))
		c.Next()
	})
	r.Use(RoutingStats(rdb))
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(status) })
	return r
}

func TestRoutingStats_RecordsSuccessRequest(t *testing.T) {
	rdb, mr := setupRoutingStatsRedis(t)
	router := newStatsTestRouter(rdb, 200)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	key := waitForStatsKey(t, mr)
	if !strings.HasPrefix(key, "stats:7:glm-5.2:zhipu:") {
		t.Fatalf("unexpected key shape: %s", key)
	}
	assertStatsField(t, mr, key, "total", 1)
	assertStatsField(t, mr, key, "success", 1)
	assertStatsField(t, mr, key, "err4xx", 0)
	assertStatsField(t, mr, key, "err5xx", 0)
	assertStatsField(t, mr, key, "rate_limited", 0)
	assertStatsField(t, mr, key, "in_tok", 100)
	assertStatsField(t, mr, key, "out_tok", 50)
	// cost = 0.01*100/1000 + 0.02*50/1000 = 0.001 + 0.001 = 0.002
	assertStatsFloat(t, mr, key, "cost", 0.002)

	// key must have a TTL set (not persisted forever)
	if ttl := mr.TTL(key); ttl <= 0 {
		t.Errorf("expected key to have TTL, got %v", ttl)
	}
}

func TestRoutingStats_Records5xxAsError(t *testing.T) {
	rdb, mr := setupRoutingStatsRedis(t)
	router := newStatsTestRouter(rdb, 500)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	key := waitForStatsKey(t, mr)
	assertStatsField(t, mr, key, "total", 1)
	assertStatsField(t, mr, key, "err5xx", 1)
	assertStatsField(t, mr, key, "success", 0)
}

func TestRoutingStats_Records429AsRateLimited(t *testing.T) {
	rdb, mr := setupRoutingStatsRedis(t)
	router := newStatsTestRouter(rdb, 429)

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	key := waitForStatsKey(t, mr)
	assertStatsField(t, mr, key, "rate_limited", 1)
	assertStatsField(t, mr, key, "err4xx", 0)
	assertStatsField(t, mr, key, "success", 0)
}

func TestRoutingStats_FailOpenOnRedisError(t *testing.T) {
	rdb, mr := setupRoutingStatsRedis(t)
	router := newStatsTestRouter(rdb, 200)
	mr.Close() // simulate Redis failure

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != 200 {
		t.Fatalf("expected 200 (fail-open), got %d", rec.Code)
	}
}

func TestRoutingStats_SkipsWhenNoModelOrProvider(t *testing.T) {
	rdb, mr := setupRoutingStatsRedis(t)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RoutingStats(rdb)) // no fake handler setting model/provider
	r.POST("/x", func(c *gin.Context) { c.Status(200) })

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/x", nil))
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("expected no stats keys on non-LLM path, got %v", keys)
	}
}

// helpers

func assertStatsField(t *testing.T, mr *miniredis.Miniredis, key, field string, want int64) {
	t.Helper()
	got := mr.HGet(key, field)
	if got == "" {
		t.Fatalf("HGet %s %s: empty (field not set)", key, field)
	}
	v, _ := strconv.ParseInt(got, 10, 64)
	if v != want {
		t.Errorf("field %s = %d, want %d", field, v, want)
	}
}

func assertStatsFloat(t *testing.T, mr *miniredis.Miniredis, key, field string, want float64) {
	t.Helper()
	got := mr.HGet(key, field)
	if got == "" {
		t.Fatalf("HGet %s %s: empty (field not set)", key, field)
	}
	v, _ := strconv.ParseFloat(got, 64)
	if math.Abs(v-want) > 1e-9 {
		t.Errorf("field %s = %f, want %f", field, v, want)
	}
}
