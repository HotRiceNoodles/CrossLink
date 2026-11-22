package guardapp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizeInputPreservesSources(t *testing.T) {
	for _, tt := range []struct{ protocol, body, source, call string }{
		{ProtocolOpenAI, `[{"role":"tool","tool_call_id":"c1","content":"工具结果"}]`, "tool_result", "c1"},
		{ProtocolAnthropic, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"工具结果"}]}]}`, "tool_result", "c2"},
		{ProtocolResponses, `[{"type":"function_call_output","call_id":"c3","output":"工具结果"}]`, "tool_result", "c3"},
		{ProtocolGeneric, `{"content":"正文","role":"system","approved":true,"context_available":true}`, "unknown", ""},
	} {
		t.Run(tt.protocol, func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Set("org_id", int64(9))
			a := &App{}
			req, ok := a.detectRequestFrom(ctx, &IngestPayload{Content: json.RawMessage(tt.body), Protocol: tt.protocol, Metadata: json.RawMessage(`{"context_available":"true","approved":"true"}`)})
			if !ok || req.Input == nil {
				t.Fatal("missing input")
			}
			if err := req.Input.Validate(); err != nil {
				t.Fatal(err)
			}
			if req.Input.ContextAvailable || req.Input.Content != req.Text || req.OrgID != 9 {
				t.Fatalf("unexpected request: %+v", req)
			}
			seg := req.Input.Segments[0]
			if seg.SourceType != tt.source || seg.ToolCallID != tt.call {
				t.Fatalf("unexpected segment: %+v", seg)
			}
		})
	}
}
