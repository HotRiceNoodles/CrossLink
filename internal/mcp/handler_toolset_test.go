package mcp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

type recordingObserver struct{ obs []domain.ToolObservation }

func (r *recordingObserver) Observe(o domain.ToolObservation) { r.obs = append(r.obs, o) }

func newToolsetCtx(key *model.APIKey) *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	if key != nil {
		c.Set("api_key", key)
	}
	return c
}

func TestObserveMCPCall_ToolsListDeclared(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	teamID, orgID := int64(5), int64(7)
	c := newToolsetCtx(&model.APIKey{ID: 9, TeamID: &teamID, OrgID: &orgID})
	req := &JSONRPCRequest{Method: MethodToolsList}
	resp := &JSONRPCResponse{Result: map[string]any{"tools": []any{
		map[string]any{"name": "read_file", "description": "read", "inputSchema": map[string]any{"type": "object"}},
		map[string]any{"description": "nameless, skipped"},
	}}}

	h.observeMCPCall(c, "fs", req, resp)

	if len(rec.obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(rec.obs))
	}
	o := rec.obs[0]
	if o.APIKeyID != 9 || o.TeamID != 5 || o.OrgID != 7 {
		t.Fatalf("identity = %+v", o)
	}
	if o.Source != "proxy" || o.Protocol != "mcp" || o.Model != "" {
		t.Fatalf("meta = %+v", o)
	}
	if len(o.Declared) != 1 {
		t.Fatalf("declared = %+v", o.Declared)
	}
	d := o.Declared[0]
	if d.Name != "mcp/fs/read_file" || d.Description != "read" {
		t.Fatalf("declared entry = %+v", d)
	}
	var schema map[string]any
	if json.Unmarshal(d.SchemaRaw, &schema) != nil || schema["type"] != "object" {
		t.Fatalf("schema = %s", d.SchemaRaw)
	}
	if len(o.Invoked) != 0 {
		t.Fatalf("tools/list must not count invocations, got %v", o.Invoked)
	}
}

func TestObserveMCPCall_ToolsListErrorOrNilSkipped(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	c := newToolsetCtx(&model.APIKey{ID: 9})
	req := &JSONRPCRequest{Method: MethodToolsList}

	h.observeMCPCall(c, "fs", req, nil)
	h.observeMCPCall(c, "fs", req, &JSONRPCResponse{Error: &JSONRPCError{Code: -1, Message: "boom"}})
	h.observeMCPCall(c, "fs", req, &JSONRPCResponse{})

	if len(rec.obs) != 0 {
		t.Fatalf("error/empty tools/list must not observe, got %+v", rec.obs)
	}
}

func TestObserveMCPCall_ToolsCallInvokedAllExits(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	c := newToolsetCtx(&model.APIKey{ID: 9})
	req := &JSONRPCRequest{Method: MethodToolsCall, Params: json.RawMessage(`{"name":"exec_shell","arguments":{}}`)}

	// resp nil (denied/blocked/failed) and resp set (success) both count.
	h.observeMCPCall(c, "sys", req, nil)
	h.observeMCPCall(c, "sys", req, &JSONRPCResponse{Result: map[string]any{}})

	if len(rec.obs) != 2 {
		t.Fatalf("expected 2 observations, got %d", len(rec.obs))
	}
	for i, o := range rec.obs {
		if len(o.Declared) != 0 {
			t.Fatalf("obs %d: tools/call must not declare, got %+v", i, o.Declared)
		}
		if len(o.Invoked) != 1 || o.Invoked[0] != "mcp/sys/exec_shell" {
			t.Fatalf("obs %d: invoked = %v", i, o.Invoked)
		}
	}
}

func TestObserveMCPCall_ToolsCallNamelessSkipped(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	c := newToolsetCtx(&model.APIKey{ID: 9})
	h.observeMCPCall(c, "sys", &JSONRPCRequest{Method: MethodToolsCall}, nil)
	h.observeMCPCall(c, "sys", &JSONRPCRequest{Method: MethodToolsCall, Params: json.RawMessage(`{}`)}, nil)
	if len(rec.obs) != 0 {
		t.Fatalf("nameless tools/call must not observe, got %+v", rec.obs)
	}
}

func TestObserveMCPCall_KeylessAndNilObserverNoOp(t *testing.T) {
	h := &Handler{toolObs: &recordingObserver{}}

	// config-auth caller: no api_key in context, nothing observed
	h.observeMCPCall(newToolsetCtx(nil), "fs", &JSONRPCRequest{Method: MethodToolsCall, Params: json.RawMessage(`{"name":"x"}`)}, nil)
	if got := h.toolObs.(*recordingObserver); len(got.obs) != 0 {
		t.Fatalf("keyless request observed: %+v", got.obs)
	}

	// Community: nil observer must be inert (no panic)
	h2 := &Handler{}
	h2.observeMCPCall(newToolsetCtx(&model.APIKey{ID: 9}), "fs", &JSONRPCRequest{Method: MethodToolsList}, &JSONRPCResponse{Result: map[string]any{"tools": []any{map[string]any{"name": "x"}}}})
}

func TestObserveMCPCall_NonToolsMethodNoOp(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	c := newToolsetCtx(&model.APIKey{ID: 9})
	h.observeMCPCall(c, "fs", &JSONRPCRequest{Method: "initialize"}, &JSONRPCResponse{Result: map[string]any{"serverInfo": map[string]any{"name": "fs"}}})
	if len(rec.obs) != 0 {
		t.Fatalf("non-tools method must not observe, got %+v", rec.obs)
	}
}

func TestObserveMCPCall_DeclaredCap(t *testing.T) {
	rec := &recordingObserver{}
	h := &Handler{toolObs: rec}
	c := newToolsetCtx(&model.APIKey{ID: 9})

	tools := make([]any, maxMCPDeclaredTools+10)
	for i := range tools {
		tools[i] = map[string]any{"name": "t" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	h.observeMCPCall(c, "fs", &JSONRPCRequest{Method: MethodToolsList}, &JSONRPCResponse{Result: map[string]any{"tools": tools}})

	if len(rec.obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(rec.obs))
	}
	if len(rec.obs[0].Declared) != maxMCPDeclaredTools {
		t.Fatalf("cap not enforced: %d", len(rec.obs[0].Declared))
	}
}
