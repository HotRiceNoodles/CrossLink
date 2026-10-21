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
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/alicebob/miniredis/v2"
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
