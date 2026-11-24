package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// Time-window admission tests for the Auth seam (design:
// docs/design/api-key-time-window-control.md §5). The evaluator itself lives
// in the commercial overlay; these tests pin the community contract — the
// seam position (both the normal and the replay branch), the response shape
// (403 + typed error code + Retry-After), and the fail-closed coercion of
// foreign verdicts.

type stubWindowPolicy struct {
	err   error
	calls int
}

func (s *stubWindowPolicy) Check(*model.APIKey, time.Time) error {
	s.calls++
	return s.err
}

func newTimeWindowTestEnv(t *testing.T) (*service.KeyService, *redis.Client, int64, string) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	cp, err := crypto.NewProvider("standard")
	if err != nil {
		t.Fatalf("crypto provider: %v", err)
	}
	ks := service.NewKeyService(repository.NewAPIKeyRepo(gdb), repository.NewAPIKeyHashRepo(gdb), gdb, cp, rdb)
	res, err := ks.Create(t.Context(), &service.CreateKeyInput{Name: "时窗测试"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return ks, rdb, res.ID, res.APIKey
}

func timeWindowServe(t *testing.T, r http.Handler, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.RemoteAddr = remoteAddr
	r.ServeHTTP(w, req)
	return w
}

func TestAuth_TimeWindowDenied403WithNextOpenAndRetryAfter(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	next := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: &service.TimeWindowError{NextOpen: next}}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "time_window_denied") {
		t.Fatalf("body must carry time_window_denied: %s", body)
	}
	if !strings.Contains(body, next.Format(time.RFC3339)) {
		t.Fatalf("body must name the next open instant %s: %s", next.Format(time.RFC3339), body)
	}
	ra := w.Header().Get("Retry-After")
	if ra == "" {
		t.Fatal("denial with a computable next open must carry Retry-After")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil || secs < 7000 || secs > 7201 {
		t.Fatalf("Retry-After should be ~7200s, got %q", ra)
	}
}

func TestAuth_TimeWindowDeniedWithoutNextOpenOmitsRetryAfter(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: &service.TimeWindowError{}}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "time_window_denied") {
		t.Fatalf("expected 403 time_window_denied, got %d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") != "" {
		t.Fatalf("zero NextOpen must not emit Retry-After, got %q", w.Header().Get("Retry-After"))
	}
}

func TestAuth_TimeWindowRetryAfterCappedAt24h(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: &service.TimeWindowError{NextOpen: time.Now().Add(48 * time.Hour)}}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
	secs, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if err != nil || secs <= 0 || secs > 86400 {
		t.Fatalf("Retry-After must be capped at 86400s, got %q", w.Header().Get("Retry-After"))
	}
}

func TestAuth_TimeWindowInvalidFailClosed403(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: &service.TimeWindowError{Invalid: true}}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "time_window_invalid") {
		t.Fatalf("expected 403 time_window_invalid (fail-closed), got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuth_TimeWindowForeignVerdictCoercedToInvalid(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: errors.New("boom")}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "time_window_invalid") {
		t.Fatalf("a non-conforming policy verdict must fail closed as time_window_invalid, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuth_TimeWindowAllowedPassesAndPolicyConsulted(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	pol := &stubWindowPolicy{}
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, pol, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusOK {
		t.Fatalf("allowed window must pass, got %d body=%s", w.Code, w.Body.String())
	}
	if pol.calls != 1 {
		t.Fatalf("policy must be consulted exactly once per request, got %d", pol.calls)
	}
}

func TestAuth_NilWindowPolicyKeepsLegacyBehavior(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, nil, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	if w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain}); w.Code != http.StatusOK {
		t.Fatalf("nil window policy must keep legacy behavior, got %d", w.Code)
	}
}

func TestAuth_TimeWindowSkippedForConfigAuthKey(t *testing.T) {
	ks, rdb, _, _ := newTimeWindowTestEnv(t)
	pol := &stubWindowPolicy{err: &service.TimeWindowError{}} // would deny if consulted
	r := setupRouter()
	r.Use(Auth("cfg-secret-key", ks, rdb, service.NoopPolicy{}, pol, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": "cfg-secret-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("config auth key is not a database key — window policy must not apply, got %d body=%s", w.Code, w.Body.String())
	}
	if pol.calls != 0 {
		t.Fatalf("policy must not be consulted on the config-key path, got %d calls", pol.calls)
	}
}

func TestAuth_TimeWindowEnforcedOnReplayPath(t *testing.T) {
	ks, rdb, keyID, _ := newTimeWindowTestEnv(t)

	run := func(pol *stubWindowPolicy) *httptest.ResponseRecorder {
		r := setupRouter()
		r.Use(Auth("", ks, rdb, service.NoopPolicy{}, pol, nil))
		r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
		token := MintReplayToken(keyID)
		// The replay branch is loopback-only by design (the token is the
		// boundary; isLocalhost is defense-in-depth).
		return timeWindowServe(t, r, "127.0.0.1:9999", map[string]string{"X-Replay-Token": token})
	}

	if w := run(&stubWindowPolicy{err: &service.TimeWindowError{}}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "time_window_denied") {
		t.Fatalf("replay must inherit the window verdict (deny is deny), got %d body=%s", w.Code, w.Body.String())
	}
	if w := run(&stubWindowPolicy{}); w.Code != http.StatusOK {
		t.Fatalf("replay inside the window must pass, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestAuth_TimeWindowDeniedRendersKeyWallClock(t *testing.T) {
	ks, rdb, _, plain := newTimeWindowTestEnv(t)
	// The denial message must name the opening instant in the key holder's
	// wall clock (Asia/Shanghai), not raw UTC. next is relative to now — a
	// hardcoded date rots the test the day after it was written.
	next := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Second)
	r := setupRouter()
	r.Use(Auth("", ks, rdb, service.NoopPolicy{}, &stubWindowPolicy{err: &service.TimeWindowError{NextOpen: next, TZ: "Asia/Shanghai"}}, nil))
	r.GET("/test", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })

	w := timeWindowServe(t, r, "10.1.2.3:5555", map[string]string{"x-api-key": plain})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	wantWallClock := next.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
	if !strings.Contains(w.Body.String(), wantWallClock) {
		t.Fatalf("message must render the next open in the key's zone (%s): %s", wantWallClock, w.Body.String())
	}
}
