package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type codexAcceptanceTransport struct {
	result any
	calls  int
}

func (tr *codexAcceptanceTransport) Send(_ context.Context, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	tr.calls++
	return &JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: tr.result}, nil
}
func (tr *codexAcceptanceTransport) Close() error               { return nil }
func (tr *codexAcceptanceTransport) Ping(context.Context) error { return nil }

func TestCodexAcceptanceMCPHandlerChecksDecodedArgumentsAndStructuredResult(t *testing.T) {
	for _, tc := range []struct {
		name, arguments string
		result          any
		wantCalls       int
	}{
		{"plain_arguments", `{"query":"SECRET"}`, map[string]any{"content": []any{}}, 0},
		{"escaped_arguments", `{"query":"SE\u0043RET"}`, map[string]any{"content": []any{}}, 0},
		{"structured_result", `{"query":"safe"}`, map[string]any{"structuredContent": map[string]any{"answer": "SECRET"}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			defer sqlDB.Close()
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
			reg := NewRegistry()
			tr := &codexAcceptanceTransport{result: tc.result}
			if err := reg.Register(t.Context(), &MCPServer{ID: 1, Name: "acceptance"}, tr); err != nil {
				t.Fatal(err)
			}
			svc := NewMCPService(nil, reg, config.MCPConfig{}, nil)
			svc.permCache.items[1] = &permCacheEntry{perms: []MCPServerPermission{}, expiresAt: time.Now().Add(time.Minute)}
			h := NewHandler(svc)
			h.SetGuardrailSvc(gs)
			r := gin.New()
			r.POST("/mcp/:server", h.HandleJSONRPC)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":` + tc.arguments + `}}`
			req := httptest.NewRequest(http.MethodPost, "/mcp/acceptance", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var resp JSONRPCResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Error == nil || tr.calls != tc.wantCalls {
				t.Fatalf("MCP safety gate bypass: calls=%d want=%d body=%s", tr.calls, tc.wantCalls, w.Body.String())
			}
		})
	}
}
