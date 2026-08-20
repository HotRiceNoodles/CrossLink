package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
)

// --- functional mocks ---

type fakePAT struct {
	validate func(ctx context.Context, plaintext string) (*model.PatToken, error)
	touched  chan int64
}

func (f *fakePAT) Validate(ctx context.Context, plaintext string) (*model.PatToken, error) {
	return f.validate(ctx, plaintext)
}

func (f *fakePAT) TouchLastUsed(ctx context.Context, id int64) error {
	if f.touched != nil {
		f.touched <- id
	}
	return nil
}

type fakeUsers struct {
	user *model.User
	err  error
}

func (f *fakeUsers) GetByID(ctx context.Context, id int64) (*model.User, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.user, nil
}

func validPat(scopes datatypes.JSON) *model.PatToken {
	return &model.PatToken{
		ID:        42,
		UserID:    7,
		Scopes:    scopes,
		Status:    1,
		ExpiresAt: time.Now().Add(time.Hour),
	}
}

func adminUser() *model.User {
	return &model.User{ID: 7, Username: "admin", RoleID: 1, Status: 1, Role: model.Role{ID: 1, Name: model.RoleAdmin}}
}

func doReq(t *testing.T, handler gin.HandlerFunc, req *http.Request) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	handler(c)
	return w, c
}

// Step 1: header extraction

func TestPATAuth_MissingHeader(t *testing.T) {
	w, _ := doReq(t, PATAuthMiddleware(&fakePAT{}, &fakeUsers{}, "provider:list"), httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestPATAuth_NonPatBearer(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer somejwtvalue")
	w, _ := doReq(t, PATAuthMiddleware(&fakePAT{}, &fakeUsers{}, "provider:list"), req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestPATAuth_CookieRejected(t *testing.T) {
	// Security: PAT auth must never fall back to cookies (CSRF defense).
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(&http.Cookie{Name: "admin_token", Value: "clpat_abc"})
	w, _ := doReq(t, PATAuthMiddleware(&fakePAT{}, &fakeUsers{}, "provider:list"), req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("cookie-only request must be 401, got %d", w.Code)
	}
}

func TestPATAuth_EmptyTokenBody(t *testing.T) {
	// Header is exactly "Bearer clpat_" — prefix-only token must be 401.
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+service.PatTokenPrefix)
	w, _ := doReq(t, PATAuthMiddleware(&fakePAT{}, &fakeUsers{}, "provider:list"), req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

// Step 2: uniform 401 without reason leakage

func TestPATAuth_ValidateErrorsUniform(t *testing.T) {
	sentinels := []error{
		service.ErrInvalidFormat,
		service.ErrPatTokenNotFound,
		service.ErrPatRevoked,
		service.ErrPatInactive,
		service.ErrPatExpired,
	}
	bodies := map[string]bool{}
	for _, sentinel := range sentinels {
		svc := &fakePAT{validate: func(ctx context.Context, p string) (*model.PatToken, error) {
			return nil, sentinel
		}}
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer clpat_x")
		w, _ := doReq(t, PATAuthMiddleware(svc, &fakeUsers{}, "provider:list"), req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%v: want 401, got %d", sentinel, w.Code)
		}
		bodies[w.Body.String()] = true
	}
	if len(bodies) != 1 {
		t.Errorf("response bodies must be identical across failure reasons, got %d variants: %v", len(bodies), bodies)
	}
	if want := `{"error":"invalid token"}`; !bodies[want] {
		t.Errorf("unexpected body %v", bodies)
	}
}

// Step 3: scope checks

func TestPATAuth_ScopeDenied(t *testing.T) {
	scopes, _ := json.Marshal([]string{"provider:list"})
	svc := &fakePAT{validate: func(ctx context.Context, p string) (*model.PatToken, error) {
		return validPat(datatypes.JSON(scopes)), nil
	}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer clpat_x")
	w, _ := doReq(t, PATAuthMiddleware(svc, &fakeUsers{user: adminUser()}, "provider:create"), req)
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

func TestPATAuth_BadScopesJSONFailClosed(t *testing.T) {
	svc := &fakePAT{validate: func(ctx context.Context, p string) (*model.PatToken, error) {
		return validPat(datatypes.JSON(`{not json`)), nil
	}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer clpat_x")
	w, _ := doReq(t, PATAuthMiddleware(svc, &fakeUsers{user: adminUser()}, "provider:list"), req)
	if w.Code != http.StatusForbidden {
		t.Errorf("unparseable scopes must fail closed 403, got %d", w.Code)
	}
}

// Step 4: user resolution

func TestPATAuth_UserNotFound(t *testing.T) {
	scopes, _ := json.Marshal([]string{"provider:list"})
	svc := &fakePAT{validate: func(ctx context.Context, p string) (*model.PatToken, error) {
		return validPat(datatypes.JSON(scopes)), nil
	}}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer clpat_x")
	w, _ := doReq(t, PATAuthMiddleware(svc, &fakeUsers{err: errors.New("nope")}, "provider:list"), req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestPATAuth_UserDisabled(t *testing.T) {
	scopes, _ := json.Marshal([]string{"provider:list"})
	svc := &fakePAT{validate: func(ctx context.Context, p string) (*model.PatToken, error) {
		return validPat(datatypes.JSON(scopes)), nil
	}}
	disabled := adminUser()
	disabled.Status = 0
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer clpat_x")
	w, _ := doReq(t, PATAuthMiddleware(svc, &fakeUsers{user: disabled}, "provider:list"), req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("disabled user must be 401, got %d", w.Code)
	}
}

// Step 5: context injection + async touch

func TestPATAuth_Success(t *testing.T) {
	scopes, _ := json.Marshal([]string{"provider:list", "provider:create"})
	touched := make(chan int64, 1)
	svc := &fakePAT{
		validate: func(ctx context.Context, p string) (*model.PatToken, error) {
			return validPat(datatypes.JSON(scopes)), nil
		},
		touched: touched,
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer clpat_x")
	w, c := doReq(t, PATAuthMiddleware(svc, &fakeUsers{user: adminUser()}, "provider:list"), req)
	if w.Code != http.StatusOK {
		t.Fatalf("want pass-through, got %d body=%s", w.Code, w.Body.String())
	}

	if v, ok := c.Get("user_id"); !ok || v.(int64) != 7 {
		t.Errorf("user_id wrong: %v", v)
	}
	if v, _ := c.Get("username"); v != "admin" {
		t.Errorf("username wrong: %v", v)
	}
	if v, ok := c.Get("role_id"); !ok || v.(int64) != 1 {
		t.Errorf("role_id wrong: %v", v)
	}
	if v, _ := c.Get("role_name"); v != model.RoleAdmin {
		t.Errorf("role_name wrong: %v", v)
	}
	if v, ok := c.Get("org_id"); !ok || v.(int64) != 0 {
		t.Errorf("org_id must be locked to 0, got %v", v)
	}
	if v, ok := c.Get("pat_id"); !ok || v.(int64) != 42 {
		t.Errorf("pat_id wrong: %v", v)
	}
	got, ok := c.Get("pat_scopes")
	if !ok {
		t.Fatal("pat_scopes missing")
	}
	want := []string{"provider:list", "provider:create"}
	gs := got.([]string)
	if len(gs) != len(want) || gs[0] != want[0] || gs[1] != want[1] {
		t.Errorf("pat_scopes wrong: %v", gs)
	}

	select {
	case id := <-touched:
		if id != 42 {
			t.Errorf("TouchLastUsed called with %d, want 42", id)
		}
	case <-time.After(2 * time.Second):
		t.Error("TouchLastUsed was not called")
	}
}
