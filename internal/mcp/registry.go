package mcp

import (
	"context"
	"log/slog"
	"sync"
)

type entry struct {
	server    *MCPServer
	transport Transport
}

type Registry struct {
	mu      sync.RWMutex
	servers map[string]*entry
}

func NewRegistry() *Registry {
	return &Registry{
		servers: make(map[string]*entry),
	}
}

func (r *Registry) Register(ctx context.Context, srv *MCPServer, tr Transport) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.servers[srv.Name]; ok && existing.transport != nil {
		existing.transport.Close()
	}

	r.servers[srv.Name] = &entry{server: srv, transport: tr}
	slog.Info("MCP server registered", "name", srv.Name, "transport", srv.TransportType)
	return nil
}

func (r *Registry) Get(name string) (*MCPServer, Transport, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	e, ok := r.servers[name]
	if !ok {
		return nil, nil, false
	}
	return e.server, e.transport, true
}

func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.servers[name]; ok {
		if e.transport != nil {
			e.transport.Close()
		}
		delete(r.servers, name)
		slog.Info("MCP server unregistered", "name", name)
	}
}

func (r *Registry) All() []MCPServer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]MCPServer, 0, len(r.servers))
	for _, e := range r.servers {
		result = append(result, *e.server)
	}
	return result
}
