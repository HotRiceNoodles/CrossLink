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
	messageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		receivedMethod = req.Method
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]string{"status": "ok"},
		})
	}))
	defer messageServer.Close()

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", messageServer.URL)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer sseServer.Close()

	tr := NewSSETransport(sseServer.URL, &noAuth{}, nil, nil)
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
