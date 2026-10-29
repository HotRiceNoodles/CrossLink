package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The log-table contract: every tools/call exit (permission-denied,
// guardrail-blocked, forward-failed, success) lands one mcp_tool_call_logs
// row on BOTH transports, with blocked calls as status -1 + BlockedBy (never
// status 0) and org/team/user identity carried from the API key object.

type logPathTransport struct {
	err error
}

func (tr *logPathTransport) Send(_ context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	if tr.err != nil {
		return nil, tr.err
	}
	return &JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"content": []any{}}}, nil
}
func (tr *logPathTransport) Close() error               { return nil }
func (tr *logPathTransport) Ping(context.Context) error { return nil }

// newLogPathHarness builds a handler over a fake transport. Set guarded=true
// for a SECRET-keyword block rule; set restricted=true for an allow list that
// excludes the test tool.
func newLogPathHarness(t *testing.T, guarded, restricted bool, tr Transport) (*gin.Engine, *MCPService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	perms := []MCPServerPermission{} // non-nil: open access without hitting the (nil) repo
	if restricted {
		perms = []MCPServerPermission{{ID: 1, ServerID: 1, PrincipalType: "key", PrincipalID: 42,
			AllowTools: datatypes.JSON(`["other_tool"]`), DenyTools: datatypes.JSON(`[]`)}}
	}

	reg := NewRegistry()
	if err := reg.Register(context.Background(), &MCPServer{ID: 1, Name: "logtest"}, tr); err != nil {
		t.Fatal(err)
	}
	svc := NewMCPService(nil, reg, config.MCPConfig{}, nil)
	svc.permCache.items[1] = &permCacheEntry{perms: perms, expiresAt: time.Now().Add(time.Minute)}
	h := NewHandler(svc)
	if guarded {
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		sqlDB, _ := db.DB()
		t.Cleanup(func() { sqlDB.Close() })
		for _, stmt := range []string{
			"CREATE TABLE system_settings (key TEXT, value TEXT)",
			"INSERT INTO system_settings VALUES ('guardrails_enabled','true')",
			"CREATE TABLE guardrail_rules (id INTEGER PRIMARY KEY, name TEXT, type TEXT, direction TEXT, enabled BOOLEAN, config TEXT, severity TEXT, action TEXT)",
			`INSERT INTO guardrail_rules VALUES (1,'synthetic','keyword_filter','both',true,'{"blocklist":["SECRET"]}','high','block')`,
		} {
			if err := db.Exec(stmt).Error; err != nil {
				t.Fatal(err)
			}
		}
		gs := guardrail.NewGuardrailService(db, nil)
		gs.RefreshSettings(t.Context())
		h.SetGuardrailSvc(gs)
	}

	teamID, orgID, userID := int64(5), int64(7), int64(9)
	key := &model.APIKey{ID: 42, TeamID: &teamID, OrgID: &orgID, CreatedByID: &userID}
	injectKey := func(c *gin.Context) { // mirrors what MCPAuth sets on db-key requests
		c.Set("api_key", key)
		c.Set("api_key_id", key.ID)
		c.Next()
	}
	r := gin.New()
	r.POST("/mcp/:server", injectKey, h.HandleJSONRPC)
	r.GET("/mcp/:server/sse", injectKey, h.HandleSSE)
	return r, svc
}

func drainLogQueue(t *testing.T, svc *MCPService) []*MCPToolCallLog {
	t.Helper()
	var logs []*MCPToolCallLog
	for {
		select {
		case l := <-svc.logQueue:
			logs = append(logs, l)
		default:
			return logs
		}
	}
}

func assertIdentity(t *testing.T, l *MCPToolCallLog) {
	t.Helper()
	if l.APIKeyID != 42 {
		t.Errorf("APIKeyID = %d, want 42", l.APIKeyID)
	}
	if l.OrgID == nil || *l.OrgID != 7 {
		t.Errorf("OrgID = %v, want 7", l.OrgID)
	}
	if l.TeamID != 5 {
		t.Errorf("TeamID = %d, want 5", l.TeamID)
	}
	if l.UserID != 9 {
		t.Errorf("UserID = %d, want 9 (key creator)", l.UserID)
	}
	if l.ServerID != 1 || l.ServerName != "logtest" {
		t.Errorf("server = %d/%s", l.ServerID, l.ServerName)
	}
	if l.ToolName != "lookup" {
		t.Errorf("ToolName = %q, want lookup", l.ToolName)
	}
}

func jsonrpcBody(args string) *bytes.Buffer {
	return bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"query":"` + args + `"}}}`)
}

func TestLogToolCall_JSONRPC_SuccessAndIdentity(t *testing.T) {
	r, svc := newLogPathHarness(t, false, false, &logPathTransport{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/logtest", jsonrpcBody("safe"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	logs := drainLogQueue(t, svc)
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logs))
	}
	l := logs[0]
	if l.Status != 1 {
		t.Errorf("Status = %d, want 1 (success)", l.Status)
	}
	if l.BlockedBy != "" {
		t.Errorf("BlockedBy = %q, want empty", l.BlockedBy)
	}
	if l.OutputSize == 0 {
		t.Errorf("OutputSize = 0, want measured result size")
	}
	assertIdentity(t, l)
}

func TestLogToolCall_JSONRPC_GuardrailBlockIsStatusMinusOne(t *testing.T) {
	r, svc := newLogPathHarness(t, true, false, &logPathTransport{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/logtest", jsonrpcBody("SECRET"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	logs := drainLogQueue(t, svc)
	if len(logs) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(logs))
	}
	l := logs[0]
	if l.Status != -1 {
		t.Errorf("Status = %d, want -1 (blocked, not error)", l.Status)
	}
	if l.BlockedBy != "guardrail" {
		t.Errorf("BlockedBy = %q, want guardrail", l.BlockedBy)
	}
	if !strings.Contains(l.ErrorMsg, "guardrail") {
		t.Errorf("ErrorMsg = %q, want guardrail context", l.ErrorMsg)
	}
	assertIdentity(t, l)
}

func TestLogToolCall_JSONRPC_PermissionDeniedIsLogged(t *testing.T) {
	r, svc := newLogPathHarness(t, false, true, &logPathTransport{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/logtest", jsonrpcBody("safe"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	logs := drainLogQueue(t, svc)
	if len(logs) != 1 {
		t.Fatalf("denied call must land a log row, got %d", len(logs))
	}
	l := logs[0]
	if l.Status != -1 || l.BlockedBy != "permission" {
		t.Errorf("Status/BlockedBy = %d/%q, want -1/permission", l.Status, l.BlockedBy)
	}
	assertIdentity(t, l)
}

func TestLogToolCall_SSE_SuccessGuardrailAndForwardFailure(t *testing.T) {
	r, svc := newLogPathHarness(t, true, false, &logPathTransport{})

	sseURL := func(args string) string {
		q := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"query":"` + args + `"}}}`
		return "/mcp/logtest/sse?request=" + url.QueryEscape(q)
	}

	// success
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, sseURL("safe"), nil))
	// guardrail blocked
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, sseURL("SECRET"), nil))

	logs := drainLogQueue(t, svc)
	if len(logs) != 2 {
		t.Fatalf("SSE path must log both exits, got %d rows", len(logs))
	}
	var ok, blocked *MCPToolCallLog
	for _, l := range logs {
		if l.Status == -1 {
			blocked = l
		} else {
			ok = l
		}
	}
	if ok == nil || blocked == nil {
		t.Fatalf("expected one success and one blocked row, got %+v", logs)
	}
	if ok.Status != 1 || ok.OutputSize == 0 {
		t.Errorf("success row = %+v", ok)
	}
	if blocked.BlockedBy != "guardrail" {
		t.Errorf("blocked row BlockedBy = %q, want guardrail", blocked.BlockedBy)
	}
	assertIdentity(t, ok)
	assertIdentity(t, blocked)

	// forward failure (fresh service so queues stay separate)
	r2, svc2 := newLogPathHarness(t, false, false, &logPathTransport{err: context.DeadlineExceeded})
	w3 := httptest.NewRecorder()
	r2.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, sseURL("safe"), nil))
	logs2 := drainLogQueue(t, svc2)
	if len(logs2) != 1 || logs2[0].Status != 0 || logs2[0].BlockedBy != "" {
		t.Fatalf("forward failure row = %+v", logs2)
	}
}
