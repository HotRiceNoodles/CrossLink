package mcp

import (
	"context"
	"testing"
)

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	srv := &MCPServer{ID: 1, Name: "test", TransportType: "http", URL: "http://localhost/mcp"}
	reg.Register(context.Background(), srv, nil)

	gotSrv, _, ok := reg.Get("test")
	if !ok {
		t.Fatal("expected to find server 'test'")
	}
	if gotSrv.ID != 1 {
		t.Errorf("ID = %d, want 1", gotSrv.ID)
	}
}

func TestRegistry_GetNotFound(t *testing.T) {
	reg := NewRegistry()
	_, _, ok := reg.Get("nonexistent")
	if ok {
		t.Error("expected not found")
	}
}

func TestRegistry_Unregister(t *testing.T) {
	reg := NewRegistry()
	srv := &MCPServer{ID: 1, Name: "test", TransportType: "http", URL: "http://localhost/mcp"}
	reg.Register(context.Background(), srv, nil)
	reg.Unregister("test")
	_, _, ok := reg.Get("test")
	if ok {
		t.Error("expected not found after unregister")
	}
}
