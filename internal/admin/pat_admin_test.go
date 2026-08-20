package admin

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// mockPatSvc implements PatSvc for testing.
type mockPatSvc struct {
	createFn func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error)
	revokeFn func(ctx context.Context, id, userID int64) error
}

func (m *mockPatSvc) Create(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
	return m.createFn(ctx, userID, allowedActions, name, scopes)
}

func (m *mockPatSvc) Revoke(ctx context.Context, id, userID int64) error {
	return m.revokeFn(ctx, id, userID)
}

// mockPatLister implements PatLister for testing.
type mockPatLister struct {
	listByUserFn func(ctx context.Context, userID int64) ([]model.PatToken, error)
}

func (m *mockPatLister) ListByUser(ctx context.Context, userID int64) ([]model.PatToken, error) {
	return m.listByUserFn(ctx, userID)
}

// mockPatRolePerms implements PatRolePerms for testing.
type mockPatRolePerms struct {
	getPermissionsFn func(ctx context.Context, roleID int64) ([]string, error)
}

func (m *mockPatRolePerms) GetPermissions(ctx context.Context, roleID int64) ([]string, error) {
	return m.getPermissionsFn(ctx, roleID)
}

// patTestRoleActions is the fixed permission list returned by the default
// rolePerms mock; Create propagation is asserted against it exactly.
var patTestRoleActions = []string{"key:list", "usage:list", "usage:read"}

// newPATHandler creates a PATAdminHandler with permissive default mocks.
// auditSvc is nil — matching the existing test pattern (keys_test.go), audit
// assertions are skipped.
func newPATHandler(patSvc *mockPatSvc, lister *mockPatLister) *PATAdminHandler {
	return &PATAdminHandler{
		patSvc: patSvc,
		lister: lister,
		rolePerms: &mockPatRolePerms{
			getPermissionsFn: func(ctx context.Context, roleID int64) ([]string, error) {
				return patTestRoleActions, nil
			},
		},
		auditSvc: nil,
	}
}

func defaultPATMocks() (*mockPatSvc, *mockPatLister) {
	patSvc := &mockPatSvc{
		createFn: func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
			return &service.CreatePatResult{
				Token:     &model.PatToken{ID: 7, UserID: userID, Name: name, Status: 1, ExpiresAt: time.Now().Add(90 * 24 * time.Hour), CreatedAt: time.Now()},
				Plaintext: "clpat_testtoken",
			}, nil
		},
		revokeFn: func(ctx context.Context, id, userID int64) error {
			return nil
		},
	}
	lister := &mockPatLister{
		listByUserFn: func(ctx context.Context, userID int64) ([]model.PatToken, error) {
			return []model.PatToken{{
				ID: 7, UserID: userID, Name: "ci-bot", TokenHash: "secret-hash",
				Scopes: []byte(`["key:list"]`), Status: 1,
				ExpiresAt: time.Now().Add(90 * 24 * time.Hour), CreatedAt: time.Now(),
			}}, nil
		},
	}
	return patSvc, lister
}

// --- Create tests ---

func TestPATAdmin_Create_Success(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	var gotAllowed []string
	patSvc.createFn = func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
		gotAllowed = allowedActions
		return &service.CreatePatResult{
			Token:     &model.PatToken{ID: 7, Name: name, Status: 1, ExpiresAt: time.Now(), CreatedAt: time.Now()},
			Plaintext: "clpat_plain",
		}, nil
	}
	h := newPATHandler(patSvc, nil)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/pats", map[string]any{
		"name":   "ci",
		"scopes": []string{"key:list"},
	})
	setAdminContext(c, 1, 2, "operator")
	h.Create(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp struct {
		Data struct {
			Token string         `json:"token"`
			Pat   map[string]any `json:"pat"`
		} `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if resp.Data.Token != "clpat_plain" {
		t.Errorf("token = %q, want plaintext shown once", resp.Data.Token)
	}
	if resp.Data.Pat["name"] != "ci" {
		t.Errorf("pat.name = %v, want ci", resp.Data.Pat["name"])
	}
	wantAllowed := license.EffectiveActions(patTestRoleActions)
	if !reflect.DeepEqual(gotAllowed, wantAllowed) {
		t.Errorf("allowedActions = %v, want %v (EffectiveActions of role perms)", gotAllowed, wantAllowed)
	}
	// auditSvc is nil per existing pattern — audit assertion skipped.
}

func TestPATAdmin_Create_ScopeExceeded(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	patSvc.createFn = func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
		return nil, service.ErrScopeExceeded
	}
	h := newPATHandler(patSvc, nil)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/pats", map[string]any{
		"name":   "ci",
		"scopes": []string{"user:create"},
	})
	setAdminContext(c, 1, 2, "operator")
	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInvalidScope {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInvalidScope)
	}
}

func TestPATAdmin_Create_EmptyName(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	patSvc.createFn = func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
		return nil, service.ErrInvalidName
	}
	h := newPATHandler(patSvc, nil)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/pats", map[string]any{
		"name": " ",
	})
	setAdminContext(c, 1, 2, "operator")
	h.Create(c)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrInvalidRequest {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrInvalidRequest)
	}
}

// TestPATAdmin_Create_RolePermError_PassesNilAllowedActions: when
// GetPermissions errors, the handler must pass nil allowedActions to the
// service (which the real service rejects — see the real-service test below).
func TestPATAdmin_Create_RolePermError_PassesNilAllowedActions(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	var gotAllowed []string
	called := false
	patSvc.createFn = func(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error) {
		called = true
		gotAllowed = allowedActions
		return &service.CreatePatResult{Token: &model.PatToken{ID: 8, Name: name, Status: 1}, Plaintext: "clpat_x"}, nil
	}
	h := newPATHandler(patSvc, nil)
	h.rolePerms = &mockPatRolePerms{
		getPermissionsFn: func(ctx context.Context, roleID int64) ([]string, error) {
			return nil, errors.New("db down")
		},
	}

	c, w := newTestContext(t, http.MethodPost, "/admin/api/pats", map[string]any{
		"name":   "ci",
		"scopes": []string{"key:list"},
	})
	setAdminContext(c, 1, 2, "operator")
	h.Create(c)

	if !called {
		t.Fatal("handler should still delegate scope enforcement to the service")
	}
	if len(gotAllowed) != 0 {
		t.Errorf("allowedActions = %v, want nil/empty when GetPermissions errors", gotAllowed)
	}
	_ = w
}

// TestPATAdmin_Create_AllowedActionsNil_RejectedByRealService verifies the
// production PatService is fail-closed: nil/empty allowedActions rejects any
// requested scope (and empty-scope PATs are rejected as meaningless).
func TestPATAdmin_Create_AllowedActionsNil_RejectedByRealService(t *testing.T) {
	for _, tc := range []struct {
		name   string
		scopes []string
	}{
		{"with scopes", []string{"key:list"}},
		{"empty scopes", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := false
			repo := &patHandlerTestRepo{create: func(t *model.PatToken) error { created = true; return nil }}
			h := &PATAdminHandler{
				patSvc: service.NewPatService(repo),
				rolePerms: &mockPatRolePerms{
					getPermissionsFn: func(ctx context.Context, roleID int64) ([]string, error) {
						return nil, errors.New("db down")
					},
				},
				auditSvc: nil,
			}
			c, w := newTestContext(t, http.MethodPost, "/admin/api/pats", map[string]any{
				"name":   "ci",
				"scopes": tc.scopes,
			})
			setAdminContext(c, 1, 2, "operator")
			h.Create(c)

			if created {
				t.Fatal("token must not be created when allowedActions is empty")
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (ErrScopeExceeded); body: %s", w.Code, w.Body.String())
			}
		})
	}
}

// patHandlerTestRepo implements service.PatTokenRepo for handler tests.
type patHandlerTestRepo struct {
	create func(t *model.PatToken) error
}

func (m *patHandlerTestRepo) FindByHash(ctx context.Context, hash string) (*model.PatToken, error) {
	return nil, service.ErrPatTokenNotFound
}
func (m *patHandlerTestRepo) Create(ctx context.Context, t *model.PatToken) error {
	return m.create(t)
}
func (m *patHandlerTestRepo) GetByID(ctx context.Context, id int64) (*model.PatToken, error) {
	return nil, service.ErrPatTokenNotFound
}
func (m *patHandlerTestRepo) Revoke(ctx context.Context, id int64) error { return nil }
func (m *patHandlerTestRepo) TouchLastUsed(ctx context.Context, id int64) error { return nil }

// --- List tests ---

func TestPATAdmin_List_DTOWhitelist(t *testing.T) {
	_, lister := defaultPATMocks()
	h := newPATHandler(nil, lister)

	c, w := newTestContext(t, http.MethodGet, "/admin/api/pats", nil)
	setAdminContext(c, 1, 2, "operator")
	h.List(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	decodeResponse(t, w, &resp)
	if len(resp.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(resp.Data))
	}
	got := resp.Data[0]
	want := map[string]bool{"id": true, "name": true, "scopes": true, "status": true, "expires_at": true, "last_used_at": true, "created_at": true}
	if len(got) != len(want) {
		t.Errorf("DTO key count = %d (%v), want %d", len(got), keysOf(got), len(want))
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("DTO missing key %q", k)
		}
	}
	for k := range got {
		if !want[k] {
			t.Errorf("DTO has unexpected key %q", k)
		}
	}
	scopes, ok := got["scopes"].([]any)
	if !ok || len(scopes) != 1 || scopes[0] != "key:list" {
		t.Errorf("scopes = %v, want [key:list]", got["scopes"])
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// --- Revoke tests ---

func TestPATAdmin_Revoke_Success(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	h := newPATHandler(patSvc, nil)

	c, w := newTestContext(t, http.MethodDelete, "/admin/api/pats/7", nil)
	setAdminContext(c, 1, 2, "operator")
	setPathParams(c, gin.Params{{Key: "id", Value: "7"}})
	h.Revoke(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["message"] != "revoked" {
		t.Errorf("message = %v, want revoked", resp["message"])
	}
}

func TestPATAdmin_Revoke_NotFound(t *testing.T) {
	patSvc, _ := defaultPATMocks()
	patSvc.revokeFn = func(ctx context.Context, id, userID int64) error {
		return errors.Join(service.ErrPatTokenNotFound, errors.New("wrapped"))
	}
	h := newPATHandler(patSvc, nil)

	c, w := newTestContext(t, http.MethodDelete, "/admin/api/pats/99", nil)
	setAdminContext(c, 1, 2, "operator")
	setPathParams(c, gin.Params{{Key: "id", Value: "99"}})
	h.Revoke(c)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	var resp map[string]any
	decodeResponse(t, w, &resp)
	if resp["error_code"] != ErrNotFound {
		t.Errorf("error_code = %v, want %s", resp["error_code"], ErrNotFound)
	}
}
