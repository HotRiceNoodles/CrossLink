package mcp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// Time-window seam on the MCP surface (design:
// docs/design/api-key-time-window-control.md §5 — deny is deny; the MCP
// surface must not widen admission). Acceptance follow-up for review #17.

type stubWindowPolicy struct {
	err   error
	calls int
}

func (s *stubWindowPolicy) Check(*model.APIKey, time.Time) error {
	s.calls++
	return s.err
}

func mcpWindowServe(t *testing.T, r *gin.Engine, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	r.ServeHTTP(w, req)
	return w
}

func newMCPWindowRouter(t *testing.T, pol service.KeyWindowPolicy) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MCPAuth("cfg-key", &mockKeyValidator{key: &model.APIKey{ID: 7}}, pol))
	r.POST("/:server", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return r
}

func TestMCPAuth_TimeWindowDenied(t *testing.T) {
	// Relative to now — a hardcoded date rots the test the day after it was
	// written (wait <= 0 → no Retry-After header).
	next := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Second)
	r := newMCPWindowRouter(t, &stubWindowPolicy{err: &service.TimeWindowError{NextOpen: next, TZ: "Asia/Shanghai"}})

	w := mcpWindowServe(t, r, map[string]string{"x-api-key": "db-key"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "time_window_denied") {
		t.Fatalf("body must carry time_window_denied: %s", body)
	}
	// The message renders the next open in the KEY's wall clock (Asia/Shanghai).
	wantWallClock := next.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
	if !strings.Contains(body, wantWallClock) {
		t.Fatalf("denial must name the next open in the key's wall clock (%s): %s", wantWallClock, body)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("denial with a computable next open must carry Retry-After")
	}
}

func TestMCPAuth_TimeWindowInvalidFailClosed(t *testing.T) {
	r := newMCPWindowRouter(t, &stubWindowPolicy{err: &service.TimeWindowError{Invalid: true}})

	w := mcpWindowServe(t, r, map[string]string{"x-api-key": "db-key"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "time_window_invalid") {
		t.Fatalf("corrupt policy must fail closed as time_window_invalid, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestMCPAuth_TimeWindowAllowedPasses(t *testing.T) {
	pol := &stubWindowPolicy{}
	r := newMCPWindowRouter(t, pol)

	if w := mcpWindowServe(t, r, map[string]string{"x-api-key": "db-key"}); w.Code != http.StatusOK {
		t.Fatalf("inside the window the MCP surface must pass, got %d", w.Code)
	}
	if pol.calls != 1 {
		t.Fatalf("policy consulted exactly once, got %d", pol.calls)
	}
}

func TestMCPAuth_TimeWindowSkippedForConfigKey(t *testing.T) {
	pol := &stubWindowPolicy{err: &service.TimeWindowError{}} // would deny if consulted
	// The config key is NOT a database key: the validator must reject it so
	// the request falls through to the config-key branch.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(MCPAuth("cfg-key", &mockKeyValidator{err: errNotADatabaseKey}, pol))
	r.POST("/:server", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	if w := mcpWindowServe(t, r, map[string]string{"x-api-key": "cfg-key"}); w.Code != http.StatusOK {
		t.Fatalf("config auth key is not a database key — window policy must not apply, got %d body=%s", w.Code, w.Body.String())
	}
	if pol.calls != 0 {
		t.Fatalf("policy must not be consulted on the config-key path, got %d calls", pol.calls)
	}
}

var errNotADatabaseKey = errors.New("not a database key")

func TestMCPAuth_NilWindowPolicyLegacy(t *testing.T) {
	r := newMCPWindowRouter(t, nil)
	if w := mcpWindowServe(t, r, map[string]string{"x-api-key": "db-key"}); w.Code != http.StatusOK {
		t.Fatalf("nil policy keeps legacy behavior, got %d", w.Code)
	}
}
