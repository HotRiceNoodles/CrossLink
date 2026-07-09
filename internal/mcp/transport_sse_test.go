package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSSETransport_Send(t *testing.T) {
	var receivedMethod string
	// Single server serves both the SSE stream and the JSON-RPC message endpoint
	// on the SAME origin — this is the realistic deployment, and validateSameOrigin
	// (now port-aware) requires the discovered message URL to share host AND port
	// with the base SSE URL.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			var req JSONRPCRequest
			json.NewDecoder(r.Body).Decode(&req)
			receivedMethod = req.Method
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  map[string]string{"status": "ok"},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: endpoint\ndata: http://%s/message\n\n", r.Host)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	tr := NewSSETransport(server.URL, &noAuth{}, nil, nil)
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  MethodPing,
	}
	resp, err := tr.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if receivedMethod != MethodPing {
		t.Errorf("received method = %q, want %q", receivedMethod, MethodPing)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	_ = tr.Close()
}
