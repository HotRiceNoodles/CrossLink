package middleware

// r61-class enforcement-bypass closure (2026-10-21 review): team/org budget
// metadata lookups now have fail-safe semantics — cold cache during a DB
// outage REJECTS (never key-only admission); an expired-but-recent snapshot
// (LKG within ttl+staleWindow) keeps ENFORCING and keeps the scope in
// reserve/settle. Covers: fresh, TTL-expiry+DB-down (LKG), beyond-window
// (unavailable), DB recovery (fresh again), and two instances sharing live
// Redis counters under LKG on both sides.
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newFailSafeCaches(t *testing.T) (*TeamCache, *OrgCache, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.Team{}, &model.Organization{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gdb.Create(&model.Team{ID: 701, Name: "t701", OrgID: ptrI64(1), BudgetLimit: 100, BudgetPeriod: "monthly"})
	gdb.Create(&model.Organization{ID: 701, Name: "o701", BudgetLimit: 200, BudgetPeriod: "monthly"})
	return NewTeamCache(repository.NewTeamRepo(gdb), context.Background()),
		NewOrgCache(repository.NewOrgRepo(gdb), context.Background()), gdb
}

func expireCacheEntry(tc *TeamCache, id int64) {
	tc.mu.Lock()
	if e, ok := tc.items[id]; ok {
		e.expiresAt = time.Now().Add(-time.Second)
	}
	tc.mu.Unlock()
}

func TestBudgetFailSafe_ColdCacheDBDown_Rejects(t *testing.T) {
	tc, oc, gdb := newFailSafeCaches(t)
	sqlDB, _ := gdb.DB()
	sqlDB.Close() // DB unreachable, cache EMPTY (true cold)
	if _, mode := tc.GetSafe(context.Background(), 701); mode != CacheModeUnavailable {
		t.Fatalf("cold+down mode = %q, want unavailable (must fail closed)", mode)
	}
	if _, mode := oc.GetSafe(context.Background(), 701); mode != CacheModeUnavailable {
		t.Fatalf("org cold+down mode = %q, want unavailable", mode)
	}
}

func TestBudgetFailSafe_TTLExpiredDBDown_EnforcesOnLKG(t *testing.T) {
	tc, _, gdb := newFailSafeCaches(t)
	if _, mode := tc.GetSafe(context.Background(), 701); mode != CacheModeFresh {
		t.Fatalf("warmup mode = %q, want fresh", mode)
	}
	expireCacheEntry(tc, 701)
	sqlDB, _ := gdb.DB()
	sqlDB.Close()
	team, mode := tc.GetSafe(context.Background(), 701)
	if mode != CacheModeStaleLKG {
		t.Fatalf("expired+down mode = %q, want stale_lkg (keep enforcing)", mode)
	}
	if team == nil || team.BudgetLimit != 100 {
		t.Fatalf("LKG snapshot wrong: %+v", team)
	}
}

func TestBudgetFailSafe_BeyondWindow_Unavailable(t *testing.T) {
	tc, _, gdb := newFailSafeCaches(t)
	tc.GetSafe(context.Background(), 701)
	expireCacheEntry(tc, 701)
	tc.mu.Lock()
	tc.items[701].expiresAt = time.Now().Add(-(tc.staleWindow + tc.ttl + time.Minute))
	tc.mu.Unlock()
	sqlDB, _ := gdb.DB()
	sqlDB.Close()
	if _, mode := tc.GetSafe(context.Background(), 701); mode != CacheModeUnavailable {
		t.Fatalf("beyond-window mode = %q, want unavailable (revocation bound passed)", mode)
	}
}

func TestBudgetFailSafe_TwoInstances_SharedLiveCountersUnderLKG(t *testing.T) {
	tc1, _, gdb := newFailSafeCaches(t)
	tc2 := NewTeamCache(repository.NewTeamRepo(gdb), context.Background())
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	for _, tc := range []*TeamCache{tc1, tc2} {
		tc.GetSafe(context.Background(), 701) // warm both
	}
	expireCacheEntry(tc1, 701)
	expireCacheEntry(tc2, 701)
	sqlDB, _ := gdb.DB()
	sqlDB.Close() // outage; both instances must keep enforcing on LKG
	for _, tc := range []*TeamCache{tc1, tc2} {
		if _, mode := tc.GetSafe(context.Background(), 701); mode != CacheModeStaleLKG {
			t.Fatal("instance not on LKG during outage")
		}
	}
	// Live Redis counters shared: both instances see the same spend.
	rdb.IncrByFloat(context.Background(), "budget:team:701:2026-10", 50)
	for _, tc := range []*TeamCache{tc1, tc2} {
		team, _ := tc.GetSafe(context.Background(), 701)
		if team == nil || team.BudgetLimit != 100 {
			t.Fatal("LKG limit lost")
		}
	}
	if got := rdb.Get(context.Background(), "budget:team:701:2026-10").Val(); got != "50" {
		t.Fatalf("shared counter = %q, want 50", got)
	}
}

func ptrI64(v int64) *int64 { return &v }

// Chain-level beyond-window contract (r65 review follow-up): with the entry
// aged past ttl+staleWindow (measured from the write via the GetSafe
// predicate expiresAt+sw+ttl) and the DB unreachable, BudgetCheck must
// answer the FULL typed 503 response contract (type=budget_metadata_
// unavailable) through a real gin chain — not just return the cache mode.
// This pins the budget branch itself, independent of which outer layer
// (auth/routing) binds first in a live outage.
func TestBudgetFailSafe_BeyondWindow_Typed503ThroughChain(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tc, oc, gdb := newFailSafeCaches(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	budgetSvc := service.NewBudgetService(rdb)
	key := &model.APIKey{ID: 701, TeamID: ptrI64(701), BudgetPeriod: "monthly", MaxBudget: 100}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("api_key", key)
		c.Set("api_key_id", key.ID)
		c.Set("request_id", "11111111-2222-3333-4444-555555555555")
		c.Next()
	})
	r.Use(BudgetCheck(budgetSvc, tc, oc))
	r.POST("/v1/chat/completions", func(c *gin.Context) { t.Fatal("handler must not run on a beyond-window rejection") })

	// Warm the entry, age it past the horizon (GetSafe predicate:
	// unavailable when now > expiresAt + staleWindow + ttl), then kill the DB.
	tc.GetSafe(context.Background(), 701)
	tc.mu.Lock()
	if e, ok := tc.items[701]; ok {
		e.expiresAt = time.Now().Add(-time.Duration(2*tc.ttl+tc.staleWindow) - time.Second)
	}
	tc.mu.Unlock()
	sqlDB, _ := gdb.DB()
	sqlDB.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 typed rejection", w.Code)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if body.Error.Type != "budget_metadata_unavailable" {
		t.Fatalf("error.type = %q, want budget_metadata_unavailable", body.Error.Type)
	}
}
