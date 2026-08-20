package admin

// pat_e2e_test.go — T10 integration test for the PAT security model.
//
// Strategy: no FullSetup. A real gin router is assembled here with the real
// production middleware chain — PATAuthMiddleware (real PatService over an
// in-memory repo + mock user resolver) + RequireAction (real PermissionCache
// over an in-memory sqlite roles schema) + real PATReadHandler (mock deps).
// This verifies middleware composition behavior, not app.go wiring (that is
// T13 manual verification).
//
// Covers design section 7 (all seven guarantees):
//  1. lifecycle: create → 200 on all four endpoints → revoke → 401 everywhere
//  2. scope enforcement: scopes=["usage:list"] → /pat/keys 403, usage 200
//  3. role layer of the triple intersection: role lacks usage:list → 403
//  4. tier layer: budget:read registered communityActions → 200 (not filtered)
//  5. default deny: no route exists outside the /admin/api/pat/* whitelist
//  6. cookie rejection: admin_token cookie without Authorization header → 401
//  7. field whitelist: responses never contain key_prefix/key_hash/url/api_key

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
)

// --- mock PatTokenRepo: in-memory, real behavior ---

type mockPatRepo struct {
	mu     sync.Mutex
	nextID int64
	byHash map[string]*model.PatToken
	touched []int64
}

func newMockPatRepo() *mockPatRepo {
	return &mockPatRepo{byHash: map[string]*model.PatToken{}}
}

func (m *mockPatRepo) FindByHash(_ context.Context, hash string) (*model.PatToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.byHash[hash]
	if !ok {
		return nil, repository.ErrPatTokenNotFound
	}
	cp := *tok
	return &cp, nil
}

func (m *mockPatRepo) Create(_ context.Context, t *model.PatToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	t.ID = m.nextID
	cp := *t
	m.byHash[t.TokenHash] = &cp
	return nil
}

func (m *mockPatRepo) GetByID(_ context.Context, id int64) (*model.PatToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tok := range m.byHash {
		if tok.ID == id {
			cp := *tok
			return &cp, nil
		}
	}
	return nil, repository.ErrPatTokenNotFound
}

func (m *mockPatRepo) Revoke(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tok := range m.byHash {
		if tok.ID == id {
			now := time.Now()
			tok.Status = 0
			tok.RevokedAt = &now
			return nil
		}
	}
	return repository.ErrPatTokenNotFound
}

func (m *mockPatRepo) TouchLastUsed(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touched = append(m.touched, id)
	return nil
}

// --- mock user resolver: admin user with role 1 ---

type mockUserResolver struct {
	user *model.User
}

func (m *mockUserResolver) GetByID(_ context.Context, _ int64) (*model.User, error) {
	return m.user, nil
}

// buildPATRouter assembles the production middleware chain over mocks.
// perms maps action→allowed for role 1 (the PAT owner's role).
func buildPATRouter(t *testing.T, perms map[string]bool) (*gin.Engine, *mockPatRepo, *service.PatService) {
	t.Helper()

	// Real PermissionCache backed by sqlite — Load() reads role_permissions.
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.RolePermission{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var rows []model.RolePermission
	for a := range perms {
		rows = append(rows, model.RolePermission{RoleID: 1, Action: a})
	}
	if len(rows) > 0 {
		if err := db.Create(&rows).Error; err != nil {
			t.Fatalf("seed role_permissions: %v", err)
		}
	}
	cache := middleware.NewPermissionCache(repository.NewRoleRepo(db))
	if err := cache.Load(); err != nil {
		t.Fatalf("load permission cache: %v", err)
	}

	// Real PatService over the in-memory repo; mock user resolver.
	patRepo := newMockPatRepo()
	patSvc := service.NewPatService(patRepo)
	users := &mockUserResolver{user: &model.User{
		ID: 1, Username: "admin", RoleID: 1, Status: 1,
		Role: model.Role{ID: 1, Name: "admin"},
	}}

	handler := NewPATReadHandler(PATReadDeps{
		KeyLister: patKeyListerFunc(func(_ context.Context, _ int64) ([]model.APIKey, error) {
			return []model.APIKey{{ID: 10, Name: "k", Status: 1, MaxBudget: 100, BudgetPeriod: "monthly"}}, nil
		}),
		UsageSummer: patUsageSummerFunc(func(_ context.Context, _ []int64) (map[int64]UsageAgg, error) {
			return map[int64]UsageAgg{10: {Requests: 3, Tokens: 300, Cost: 0.5}}, nil
		}),
		UsageAgg: patUsageAggFunc(func(_ context.Context, _ time.Time) ([]DailyAgg, error) {
			return []DailyAgg{{Date: "2026-08-20", Requests: 3, Tokens: 300, Cost: 0.5}}, nil
		}),
		BudgetSpent: patBudgetSpentFunc(func(_ context.Context, _, _, _ string) float64 { return 50 }),
		Health:      patHealthFunc(func() []provider.ProviderHealthSnapshot { return nil }),
		Version:     "test",
	})

	r := gin.New()
	patGroup := r.Group("/admin/api/pat")
	patGroup.GET("/keys",
		middleware.PATAuthMiddleware(patSvc, users, "key:list"),
		middleware.RequireAction(cache, "key:list"),
		handler.Keys)
	patGroup.GET("/usage/summary",
		middleware.PATAuthMiddleware(patSvc, users, "usage:list"),
		middleware.RequireAction(cache, "usage:list"),
		handler.Usage)
	patGroup.GET("/budgets/status",
		middleware.PATAuthMiddleware(patSvc, users, "budget:read"),
		middleware.RequireAction(cache, "budget:read"),
		handler.Budgets)
	patGroup.GET("/health",
		middleware.PATAuthMiddleware(patSvc, users, "health:read"),
		middleware.RequireAction(cache, "health:read"),
		handler.Health)

	return r, patRepo, patSvc
}

var patEndpoints = []string{
	"/admin/api/pat/keys",
	"/admin/api/pat/usage/summary",
	"/admin/api/pat/budgets/status",
	"/admin/api/pat/health",
}

func doPAT(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// 1. Lifecycle: full-permission PAT works on all four endpoints; after
// Revoke the same token gets 401 everywhere.
func TestPATE2E_Lifecycle(t *testing.T) {
	r, repo, svc := buildPATRouter(t, map[string]bool{
		"key:list": true, "usage:list": true, "budget:read": true, "health:read": true,
	})

	res, err := svc.Create(context.Background(), 1, []string{"key:list", "usage:list", "budget:read", "health:read"},
		"e2e", []string{"key:list", "usage:list", "budget:read", "health:read"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tok := res.Plaintext

	for _, ep := range patEndpoints {
		if w := doPAT(r, ep, tok); w.Code != http.StatusOK {
			t.Errorf("%s: got %d want 200 (body %s)", ep, w.Code, w.Body.String())
		}
	}

	if err := svc.Revoke(context.Background(), repo.nextID, 1); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	for _, ep := range patEndpoints {
		if w := doPAT(r, ep, tok); w.Code != http.StatusUnauthorized {
			t.Errorf("%s after revoke: got %d want 401", ep, w.Code)
		}
	}
}

// 2. Scope enforcement (PAT layer of the triple intersection): a PAT scoped
// to usage:list only cannot reach /pat/keys.
func TestPATE2E_ScopeExceeded(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{
		"key:list": true, "usage:list": true,
	})

	res, err := svc.Create(context.Background(), 1, []string{"key:list", "usage:list"},
		"scoped", []string{"usage:list"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if w := doPAT(r, "/admin/api/pat/keys", res.Plaintext); w.Code != http.StatusForbidden {
		t.Errorf("keys with usage-only scope: got %d want 403", w.Code)
	}
	if w := doPAT(r, "/admin/api/pat/usage/summary", res.Plaintext); w.Code != http.StatusOK {
		t.Errorf("usage with usage scope: got %d want 200", w.Code)
	}
}

// 3. Role layer: role 1 lacks usage:list in PermissionCache even though the
// PAT scope grants it → 403.
func TestPATE2E_RoleDowngrade(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{"key:list": true}) // no usage:list

	res, err := svc.Create(context.Background(), 1, []string{"key:list", "usage:list"},
		"role-test", []string{"key:list", "usage:list"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	w := doPAT(r, "/admin/api/pat/usage/summary", res.Plaintext)
	if w.Code != http.StatusForbidden {
		t.Errorf("usage with scope but role lacks usage:list: got %d want 403", w.Code)
	}
	// Sanity: key:list still works — the role layer is per-action.
	if w := doPAT(r, "/admin/api/pat/keys", res.Plaintext); w.Code != http.StatusOK {
		t.Errorf("keys: got %d want 200", w.Code)
	}
}

// 4. Tier layer: budget:read is registered in communityActions so
// license.TierAllowsAction passes on Community; scope+role both grant it.
func TestPATE2E_TierCommunityAllowsBudgetRead(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{"budget:read": true})

	res, err := svc.Create(context.Background(), 1, []string{"budget:read"},
		"budget", []string{"budget:read"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	w := doPAT(r, "/admin/api/pat/budgets/status", res.Plaintext)
	if w.Code != http.StatusOK {
		t.Errorf("budgets on Community tier: got %d want 200 (body %s)", w.Code, w.Body.String())
	}
}

// 5. Default deny: the router exposes only the four whitelisted /admin/api/pat/*
// routes — a PAT token against any other admin path has no route to hit
// (covers "no routes outside the whitelist"; app.go wiring is T13).
func TestPATE2E_DefaultDenyOutsideWhitelist(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{"key:list": true})
	res, err := svc.Create(context.Background(), 1, []string{"key:list"}, "dd", []string{"key:list"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	var paths []string
	for _, ri := range r.Routes() {
		paths = append(paths, ri.Path)
	}
	sort.Strings(paths)
	if len(paths) != 4 {
		t.Errorf("router exposes %d routes, want exactly the 4 PAT whitelist routes: %v", len(paths), paths)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/admin/api/pat/") {
			t.Errorf("route outside PAT whitelist registered: %s", p)
		}
	}
	// A PAT token against a non-whitelisted admin path never reaches a handler.
	if w := doPAT(r, "/admin/api/keys", res.Plaintext); w.Code != http.StatusNotFound {
		t.Errorf("PAT on non-whitelisted path: got %d want 404", w.Code)
	}
}

// 6. Cookie rejection: PATAuthMiddleware must ignore cookies entirely — an
// admin_token cookie without an Authorization header is 401.
func TestPATE2E_CookieRejected(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{"key:list": true})
	res, err := svc.Create(context.Background(), 1, []string{"key:list"}, "cookie", []string{"key:list"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/pat/keys", nil)
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: res.Plaintext})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cookie-only auth: got %d want 401", w.Code)
	}
}

// 7. Field whitelist: successful responses from all four endpoints never
// contain sensitive field substrings (case-sensitive).
func TestPATE2E_FieldWhitelist(t *testing.T) {
	r, _, svc := buildPATRouter(t, map[string]bool{
		"key:list": true, "usage:list": true, "budget:read": true, "health:read": true,
	})
	res, err := svc.Create(context.Background(), 1,
		[]string{"key:list", "usage:list", "budget:read", "health:read"},
		"fields", []string{"key:list", "usage:list", "budget:read", "health:read"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	forbidden := []string{"key_prefix", "key_hash", "url", "api_key"}
	for _, ep := range patEndpoints {
		w := doPAT(r, ep, res.Plaintext)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: got %d want 200", ep, w.Code)
		}
		var body any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid JSON: %v", ep, err)
		}
		deepAssertNoSubstring(t, ep, body, forbidden, w.Body.String())
	}
}

// deepAssertNoSubstring walks the decoded JSON tree asserting no string key
// or value contains any forbidden substring.
func deepAssertNoSubstring(t *testing.T, ep string, node any, forbidden []string, raw string) {
	t.Helper()
	var walk func(n any, path string)
	walk = func(n any, path string) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				for _, f := range forbidden {
					if strings.Contains(k, f) {
						t.Errorf("%s: forbidden field %q at %s", ep, k, path)
					}
				}
				walk(child, path+"."+k)
			}
		case []any:
			for i, child := range v {
				walk(child, path)
				_ = i
			}
		case string:
			for _, f := range forbidden {
				if strings.Contains(v, f) {
					t.Errorf("%s: forbidden value %q at %s", ep, v, path)
				}
			}
		}
	}
	walk(node, "$")
	// Belt and braces: raw body substring check (catches anything the walker misses).
	for _, f := range forbidden {
		if strings.Contains(raw, f) {
			t.Errorf("%s: raw body contains forbidden substring %q", ep, f)
		}
	}
}

// ErrPatTokenNotFound must equal repository.ErrPatTokenNotFound so the mock
// repo behaves like the real one.
func TestPATE2E_MockRepoNotFoundSemantics(t *testing.T) {
	if !errors.Is(repository.ErrPatTokenNotFound, gorm.ErrRecordNotFound) {
		t.Fatal("repository.ErrPatTokenNotFound must wrap gorm.ErrRecordNotFound for Validate to map it")
	}
}
