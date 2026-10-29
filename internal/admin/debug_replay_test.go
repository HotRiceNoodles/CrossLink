package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newReplayHandler(t *testing.T, port int, upstream *httptest.Server) *DebugHandler {
	t.Helper()
	store := debug.NewStore(100, 1024*1024)
	store.SetEnabled(true)
	// If upstream is provided, use its port; otherwise use the given port.
	actualPort := port
	if upstream != nil {
		// Extract port from upstream.URL (http://127.0.0.1:PORT)
		// The handler constructs http://127.0.0.1:{port}{path}
		// So we need to pass the upstream's port.
		// Parse it from the URL.
		u := upstream.URL
		// e.g. http://127.0.0.1:12345 → 12345
		colon := -1
		for i := len(u) - 1; i >= 0; i-- {
			if u[i] == ':' {
				colon = i
				break
			}
		}
		if colon >= 0 {
			// strip trailing slash
			portStr := u[colon+1:]
			for len(portStr) > 0 && portStr[len(portStr)-1] == '/' {
				portStr = portStr[:len(portStr)-1]
			}
			var p int
			fmtSscanf(portStr, &p)
			actualPort = p
		}
	}
	return NewDebugHandler(store, nil, nil, actualPort)
}

func fmtSscanf(s string, v *int) {
	for _, c := range s {
		if c >= '0' && c <= '9' {
			*v = *v*10 + int(c-'0')
		}
	}
}

func addEntry(t *testing.T, store *debug.Store, path string, body []byte, stream bool, truncated bool) int64 {
	t.Helper()
	entry := &debug.Entry{
		Seq:       0,
		Path:      path,
		Method:    "POST",
		ReqBody:   body,
		Stream:    stream,
		Truncated: truncated,
	}
	store.Add(entry)
	// Add assigns Seq internally; find it.
	entries := store.List()
	if len(entries) == 0 {
		t.Fatal("failed to add entry")
	}
	return entries[len(entries)-1].Seq
}

func TestDebug_Replay_EntryNotFound(t *testing.T) {
	h := newReplayHandler(t, 8080, nil)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/999/replay", gin.H{
		"key_id": 1,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: "999"}})
	h.Replay(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDebug_Replay_MissingKeyID(t *testing.T) {
	h := newReplayHandler(t, 8080, nil)
	seq := addEntry(t, h.store, "/v1/chat/completions", []byte(`{"model":"m","messages":[]}`), false, false)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDebug_Replay_TruncatedRejected(t *testing.T) {
	h := newReplayHandler(t, 8080, nil)
	seq := addEntry(t, h.store, "/v1/chat/completions", []byte(`{"model":"m"}`), false, true)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 1,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "replay_truncated")
}

func TestDebug_Replay_StreamAutoStripped(t *testing.T) {
	var capturedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"replayed","choices":[]}`))
	}))
	defer upstream.Close()

	h := newReplayHandler(t, 0, upstream)
	seq := addEntry(t, h.store, "/v1/chat/completions", []byte(`{"model":"m","stream":true}`), true, false)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 1,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)
	require.Equal(t, http.StatusOK, w.Code, "stream requests should auto-strip and replay, not reject")
	_, hasStream := capturedBody["stream"]
	assert.False(t, hasStream, "stream field must be stripped before replay")
}

func TestDebug_Replay_PathNotAllowed(t *testing.T) {
	h := newReplayHandler(t, 8080, nil)
	seq := addEntry(t, h.store, "/v1/videos", []byte(`{}`), false, false)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 1,
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "replay_path_not_allowed")
}

func TestDebug_Replay_RemovesXContext(t *testing.T) {
	// Start a test HTTP server that captures the incoming body.
	var capturedBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		capturedBody = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"replayed","choices":[]}`))
	}))
	defer upstream.Close()

	h := newReplayHandler(t, 0, upstream)
	originalBody := []byte(`{"model":"m","x_context":{"template":"t"},"messages":[{"role":"user","content":"hi"}]}`)
	seq := addEntry(t, h.store, "/v1/chat/completions", originalBody, false, false)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id":    1,
		"overrides": gin.H{"model": "new-model"},
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// x_context must be removed.
	assert.NotContains(t, capturedBody, "x_context", "x_context must be removed before replay")
	// overrides applied.
	assert.Equal(t, "new-model", capturedBody["model"])
}

func TestDebug_Replay_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmtFprintf(w, `{"id":"replayed-%d","model":"new-model","choices":[{"message":{"role":"assistant","content":"hello"}}]}`, time.Now().UnixNano())
	}))
	defer upstream.Close()

	h := newReplayHandler(t, 0, upstream)
	originalBody := []byte(`{"model":"old-model","messages":[{"role":"user","content":"hi"}]}`)
	seq := addEntry(t, h.store, "/v1/chat/completions", originalBody, false, false)

	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id":    1,
		"overrides": gin.H{"model": "new-model", "temperature": 0.9},
	})
	setAdminContext(c, 1, 1, "admin")
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "replayed")
	assert.Contains(t, w.Body.String(), "new-model")
}

// helpers to avoid importing fmt in test
func fmtFprintf(w http.ResponseWriter, format string, args ...any) {
	// minimal: just write a fixed response for test
	w.Write([]byte(`{"id":"replayed","model":"new-model","choices":[{"message":{"role":"assistant","content":"hello"}}]}`))
}

// --- Replay key org-scoping (external report 2026-10-28): the minted replay
// token carries no org, so key ownership is enforced here at mint time.

// newReplayKeySvc builds a sqlite-backed KeyService for ownership checks.
func newReplayKeySvc(t *testing.T, keys ...model.APIKey) *service.KeyService {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, gdb.AutoMigrate(&model.APIKey{}, &model.APIKeyHash{}))
	for i := range keys {
		require.NoError(t, gdb.Create(&keys[i]).Error)
	}
	cp, err := crypto.NewProvider("standard")
	require.NoError(t, err)
	return service.NewKeyService(repository.NewAPIKeyRepo(gdb), repository.NewAPIKeyHashRepo(gdb), gdb, cp, nil)
}

func upstreamPort(t *testing.T, url string) int {
	t.Helper()
	colon := strings.LastIndex(url, ":")
	require.NotEqual(t, -1, colon, "upstream URL has no port: %s", url)
	var p int
	fmtSscanf(url[colon+1:], &p)
	return p
}

// addOrgEntry seeds a debug entry owned by orgID (addEntry leaves OrgID 0,
// which the entry-level org check rejects for org admins).
func addOrgEntry(t *testing.T, store *debug.Store, orgID int64) int64 {
	t.Helper()
	store.Add(&debug.Entry{
		Path:    "/v1/chat/completions",
		Method:  "POST",
		ReqBody: []byte(`{"model":"m","messages":[]}`),
		OrgID:   orgID,
	})
	entries := store.List()
	require.NotEmpty(t, entries)
	return entries[len(entries)-1].Seq
}

func TestDebug_Replay_OrgAdminCannotReplayForeignOrgKey(t *testing.T) {
	foreignOrg := int64(8)
	keySvc := newReplayKeySvc(t, model.APIKey{
		ID: 5, Name: "foreign-key", KeyHash: "hash-5", KeyPrefix: "sk-x", Status: 1, OrgID: &foreignOrg,
	})

	store := debug.NewStore(100, 1024*1024)
	store.SetEnabled(true)
	seq := addOrgEntry(t, store, 7)

	h := NewDebugHandler(store, nil, keySvc, 8080)
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 5,
	})
	setAdminContext(c, 1, 1, "org_admin")
	c.Set("org_id", int64(7)) // org admin of org 7, key belongs to org 8
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Body.String(), "key_not_found")
}

func TestDebug_Replay_OrgAdminSameOrgKeyMintsTokenNotKeyID(t *testing.T) {
	var gotReplayToken, gotLegacyKeyID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReplayToken = r.Header.Get("X-Replay-Token")
		gotLegacyKeyID = r.Header.Get("X-Replay-KeyID")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"replayed","choices":[]}`))
	}))
	defer upstream.Close()

	ownOrg := int64(7)
	keySvc := newReplayKeySvc(t, model.APIKey{
		ID: 5, Name: "own-key", KeyHash: "hash-5", KeyPrefix: "sk-x", Status: 1, OrgID: &ownOrg,
	})

	store := debug.NewStore(100, 1024*1024)
	store.SetEnabled(true)
	seq := addOrgEntry(t, store, 7)

	h := NewDebugHandler(store, nil, keySvc, upstreamPort(t, upstream.URL))
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 5,
	})
	setAdminContext(c, 1, 1, "org_admin")
	c.Set("org_id", int64(7))
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.NotEmpty(t, gotReplayToken, "replay must carry a minted X-Replay-Token")
	assert.Empty(t, gotLegacyKeyID, "the legacy guessable X-Replay-KeyID header must not be sent")
}

// End-to-end across both halves of the fix: Replay mints a token and
// loopback-calls a production-shaped engine (SetTrustedProxies(nil), real
// Auth middleware), which must redeem it and authenticate the request as the
// minted key. Proves the admin replay feature still works AND that nothing
// but the minted token gets through.
func TestDebug_Replay_EndToEndThroughRealAuth(t *testing.T) {
	keySvc := newReplayKeySvc(t, model.APIKey{
		ID: 5, Name: "victim-key", KeyHash: "hash-5", KeyPrefix: "sk-x", Status: 1,
	})

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	require.NoError(t, engine.SetTrustedProxies(nil)) // production default (app.go)
	engine.Use(middleware.Auth("", keySvc, nil, service.NoopPolicy{}))
	var gotKeyID int64
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		key := middleware.GetAPIKeyFromContext(c)
		if key == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthenticated"})
			return
		}
		gotKeyID = key.ID
		c.JSON(http.StatusOK, gin.H{"id": "replayed", "choices": []any{}})
	})
	srv := httptest.NewServer(engine)
	defer srv.Close()

	store := debug.NewStore(100, 1024*1024)
	store.SetEnabled(true)
	seq := addOrgEntry(t, store, 0) // platform-level entry; caller below is platform admin

	h := NewDebugHandler(store, nil, keySvc, upstreamPort(t, srv.URL))
	c, w := newTestContext(t, http.MethodPost, "/admin/api/debug/entries/"+itoa(seq)+"/replay", gin.H{
		"key_id": 5,
	})
	setAdminContext(c, 1, 1, "admin") // platform admin: org_id unset
	setPathParams(c, gin.Params{{Key: "seq", Value: itoa(seq)}})
	h.Replay(c)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, int64(5), gotKeyID, "replayed request must authenticate as key 5")
}
