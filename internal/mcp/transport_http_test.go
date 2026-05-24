package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPTransport_Send(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server decode: %v", err)
			http.Error(w, err.Error(), 400)
			return
		}
		resp := JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]string{"method": req.Method},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	tr := NewHTTPTransport(server.URL, &noAuth{}, nil, nil)
	req := &JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  MethodToolsList,
	}
	resp, err := tr.Send(context.Background(), req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	result, ok := resp.Result.(map[string]interface{})
	if !ok {
		t.Fatal("result is not a map")
	}
	if result["method"] != MethodToolsList {
		t.Errorf("method = %v, want %v", result["method"], MethodToolsList)
	}
}

func TestHTTPTransport_Ping(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req JSONRPCRequest
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(JSONRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  map[string]interface{}{},
		})
	}))
	defer server.Close()

	tr := NewHTTPTransport(server.URL, &noAuth{}, nil, nil)
	if err := tr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
