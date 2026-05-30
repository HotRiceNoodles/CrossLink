package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/crosslink/internal/config"
	"golang.org/x/sync/singleflight"
)

const logQueueSize = 1024

type MCPService struct {
	repo               *MCPRepo
	registry           *Registry
	cfg                config.MCPConfig
	encStore           Encrypter
	toolCache          *toolCache
	permCache          *permCache
	sharedTransport    *http.Transport
	transportFactories map[string]func(*MCPServer) Transport
	logQueue           chan *MCPToolCallLog
	toolSF             singleflight.Group
}

type permCacheEntry struct {
	perms     []MCPServerPermission
	expiresAt time.Time
}

type permCache struct {
	mu    sync.RWMutex
	items map[int64]*permCacheEntry
}

type toolCacheEntry struct {
	tools     []interface{}
	expiresAt time.Time
}

const maxCacheEntries = 1000

type toolCache struct {
	mu    sync.RWMutex
	items map[string]*toolCacheEntry
}

// Encrypter encrypts/decrypts sensitive strings. Matches *secret.EncryptedDBStore.
type Encrypter interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(encrypted string) (string, error)
	IsEncrypted(s string) bool
}

func NewMCPService(repo *MCPRepo, registry *Registry, cfg config.MCPConfig, encStore Encrypter) *MCPService {
	maxIdle := cfg.HTTPMaxIdleConns
	if maxIdle == 0 {
		maxIdle = 10
	}
	return &MCPService{
		repo:     repo,
		registry: registry,
		cfg:      cfg,
		encStore: encStore,
		toolCache: &toolCache{items: make(map[string]*toolCacheEntry)},
		permCache: &permCache{items: make(map[int64]*permCacheEntry)},
		logQueue: make(chan *MCPToolCallLog, logQueueSize),
		sharedTransport: &http.Transport{
			MaxIdleConnsPerHost: maxIdle,
			MaxIdleConns:        maxIdle,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func (s *MCPService) RegisterTransportFactory(transportType string, factory func(*MCPServer) Transport) {
	if s.transportFactories == nil {
		s.transportFactories = make(map[string]func(*MCPServer) Transport)
	}
	s.transportFactories[transportType] = factory
}

func (s *MCPService) CreateServer(ctx context.Context, srv *MCPServer) error {
	if err := ValidateServerName(srv.Name); err != nil {
		return err
	}
	if srv.TransportType == "stdio" {
		if _, ok := s.transportFactories["stdio"]; !ok {
			return fmt.Errorf("stdio transport requires Pro or Enterprise license")
		}
	}
	if srv.TransportType == "http" || srv.TransportType == "sse" {
		if srv.URL == "" {
			return fmt.Errorf("url is required for HTTP/SSE transport")
		}
	}

	if err := s.repo.CreateWithLimit(ctx, srv, s.cfg.MaxServers); err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	tr := s.createTransport(srv)
	if err := s.registry.Register(ctx, srv, tr); err != nil {
		return fmt.Errorf("register server: %w", err)
	}

	return nil
}

func (s *MCPService) GetServer(ctx context.Context, orgID int64, id int64) (*MCPServer, error) {
	return s.repo.GetByID(ctx, orgID, id)
}

func (s *MCPService) ListServers(ctx context.Context, orgID int64) ([]MCPServer, error) {
	return s.repo.List(ctx, orgID)
}

func (s *MCPService) UpdateServer(ctx context.Context, srv *MCPServer) error {
	if err := s.repo.Update(ctx, srv); err != nil {
		return err
	}
	s.InvalidateToolCache(srv.Name)
	tr := s.createTransport(srv)
	return s.registry.Register(ctx, srv, tr)
}

func (s *MCPService) DeleteServer(ctx context.Context, orgID int64, id int64) error {
	srv, err := s.repo.GetByID(ctx, orgID, id)
	if err != nil {
		return err
	}
	s.InvalidateToolCache(srv.Name)
	s.registry.Unregister(srv.Name)
	return s.repo.Delete(ctx, id)
}

func (s *MCPService) ForwardRequest(ctx context.Context, serverName string, req *JSONRPCRequest) (*JSONRPCResponse, error) {
	_, tr, ok := s.registry.Get(serverName)
	if !ok {
		return nil, fmt.Errorf("MCP server %q not found or inactive", serverName)
	}
	if tr == nil {
		return nil, fmt.Errorf("MCP server %q has no transport", serverName)
	}

	timeout := s.cfg.RequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return tr.Send(ctx, req)
}

func (s *MCPService) TestServer(ctx context.Context, orgID int64, id int64) error {
	srv, err := s.repo.GetByID(ctx, orgID, id)
	if err != nil {
		return err
	}
	_, tr, ok := s.registry.Get(srv.Name)
	if !ok || tr == nil {
		tr = s.createTransport(srv)
	}
	if tr == nil {
		return fmt.Errorf("no transport available for server %q", srv.Name)
	}
	return tr.Ping(ctx)
}

func (s *MCPService) GetServerTools(ctx context.Context, serverName string) ([]interface{}, error) {
	ttl := s.cfg.ToolCacheTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}

	s.toolCache.mu.RLock()
	if entry, ok := s.toolCache.items[serverName]; ok && time.Now().Before(entry.expiresAt) {
		s.toolCache.mu.RUnlock()
		return entry.tools, nil
	}
	s.toolCache.mu.RUnlock()

	v, err, _ := s.toolSF.Do("tools:"+serverName, func() (interface{}, error) {
		req := &JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      json.RawMessage(`"tools-list"`),
			Method:  MethodToolsList,
		}
		resp, err := s.ForwardRequest(ctx, serverName, req)
		if err != nil {
			return nil, err
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("tools/list error: %s", resp.Error.Message)
		}
		resultBytes, _ := json.Marshal(resp.Result)
		var result struct {
			Tools []interface{} `json:"tools"`
		}
		if err := json.Unmarshal(resultBytes, &result); err != nil {
			return nil, fmt.Errorf("parse tools: %w", err)
		}

		s.toolCache.mu.Lock()
		if len(s.toolCache.items) > maxCacheEntries {
			now := time.Now()
			for k, v := range s.toolCache.items {
				if now.After(v.expiresAt) {
					delete(s.toolCache.items, k)
				}
			}
		}
		s.toolCache.items[serverName] = &toolCacheEntry{
			tools:     result.Tools,
			expiresAt: time.Now().Add(ttl),
		}
		s.toolCache.mu.Unlock()

		if srv, _, ok := s.registry.Get(serverName); ok {
			if err := s.repo.UpdateToolCount(ctx, srv.ID, len(result.Tools)); err != nil {
				slog.Error("failed to update tool_count", "server", serverName, "error", err)
			}
		}

		return result.Tools, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]interface{}), nil
}

func (s *MCPService) InvalidateToolCache(serverName string) {
	s.toolCache.mu.Lock()
	delete(s.toolCache.items, serverName)
	s.toolCache.mu.Unlock()
}

func (s *MCPService) InvalidatePermCache(serverID int64) {
	s.permCache.mu.Lock()
	delete(s.permCache.items, serverID)
	s.permCache.mu.Unlock()
}

func (s *MCPService) LoadFromDB(ctx context.Context) error {
	servers, err := s.repo.List(ctx, 0)
	if err != nil {
		return err
	}
	for i := range servers {
		srv := &servers[i]
		tr := s.createTransport(srv)
		if err := s.registry.Register(ctx, srv, tr); err != nil {
			slog.Error("failed to register MCP server", "name", srv.Name, "error", err)
		} else {
			slog.Info("loaded MCP server", "name", srv.Name, "transport", srv.TransportType)
		}
	}
	return nil
}

// errorTransport always returns an error. Used when auth config is invalid
// to prevent sending requests without authentication.
type errorTransport struct {
	err error
}

func (t *errorTransport) Send(_ context.Context, _ *JSONRPCRequest) (*JSONRPCResponse, error) {
	return nil, t.err
}

func (t *errorTransport) Ping(_ context.Context) error { return t.err }

func (t *errorTransport) Close() error { return nil }

func (s *MCPService) createTransport(srv *MCPServer) Transport {
	auth, err := NewAuthenticator(srv.AuthType, json.RawMessage(srv.AuthConfig), s.decryptValue, s.sharedTransport)
	if err != nil {
		slog.Error("create MCP authenticator", "server", srv.Name, "error", err)
		// Mark server unhealthy instead of sending requests without auth
		_ = s.repo.UpdateHealthStatus(context.Background(), srv.ID, -1)
		return &errorTransport{err: fmt.Errorf("auth config invalid: %w", err)}
	}

	var headers map[string]string
	if srv.CustomHeaders != nil {
		json.Unmarshal(srv.CustomHeaders, &headers)
	}

	switch srv.TransportType {
	case "http":
		return NewHTTPTransport(srv.URL, auth, headers, s.sharedTransport)
	case "sse":
		return NewSSETransport(srv.URL, auth, headers, s.sharedTransport)
	default:
		if fn, ok := s.transportFactories[srv.TransportType]; ok {
			return fn(srv)
		}
		slog.Warn("unsupported transport type", "type", srv.TransportType, "server", srv.Name)
		return nil
	}
}

// decryptValue returns plaintext for enc:// values, or the value unchanged.
func (s *MCPService) decryptValue(val string) string {
	if s.encStore != nil && s.encStore.IsEncrypted(val) {
		if plain, err := s.encStore.Decrypt(val); err == nil {
			return plain
		}
	}
	return val
}

// SetEncStore allows late binding of the encryption store (called from app.go after key resolution).
func (s *MCPService) SetEncStore(encStore Encrypter) {
	s.encStore = encStore
}

func (s *MCPService) LogToolCall(_ context.Context, log *MCPToolCallLog) {
	select {
	case s.logQueue <- log:
	default:
		slog.Warn("MCP log queue full, dropping tool call log", "server", log.ServerName, "tool", log.ToolName)
	}
}

func (s *MCPService) StartLogWorker(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				// Drain remaining logs
				for {
					select {
					case log := <-s.logQueue:
						if err := s.repo.LogToolCall(ctx, log); err != nil {
							slog.Error("failed to log MCP tool call", "server", log.ServerName, "error", err)
						}
					default:
						return
					}
				}
			case log := <-s.logQueue:
				if err := s.repo.LogToolCall(ctx, log); err != nil {
					slog.Error("failed to log MCP tool call", "server", log.ServerName, "tool", log.ToolName, "error", err)
				}
			}
		}
	}()
}

// StartHealthChecks launches a background goroutine that periodically pings all registered MCP servers.
func (s *MCPService) StartHealthChecks(ctx context.Context) {
	interval := s.cfg.HealthCheckInterval
	if interval == 0 {
		interval = 30 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Run once immediately on start
		s.runHealthChecks(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runHealthChecks(ctx)
			}
		}
	}()
}

func (s *MCPService) runHealthChecks(ctx context.Context) {
	servers := s.registry.All()
	sem := make(chan struct{}, 10) // max 10 concurrent health checks
	var wg sync.WaitGroup
	for _, srv := range servers {
		if !srv.Enabled {
			continue
		}
		_, tr, ok := s.registry.Get(srv.Name)
		if !ok || tr == nil {
			continue
		}
		wg.Add(1)
		go func(name string, id int64, tr Transport) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			healthStatus := int16(1) // healthy
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if err := tr.Ping(pingCtx); err != nil {
				healthStatus = -1
				slog.Warn("MCP health check failed", "server", name, "error", err)
			}
			cancel()
			if err := s.repo.UpdateHealthStatus(ctx, id, healthStatus); err != nil {
				slog.Error("failed to update MCP health status", "server", name, "error", err)
			}
			if healthStatus == 1 {
				if _, err := s.GetServerTools(ctx, name); err != nil {
					slog.Debug("failed to refresh tool list during health check", "server", name, "error", err)
				}
			}
		}(srv.Name, srv.ID, tr)
	}
	wg.Wait()
}

func (s *MCPService) StartLogCleanup(ctx context.Context) {
	days := s.cfg.LogRetentionDays
	if days <= 0 {
		days = 180
	}
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().AddDate(0, 0, -days)
				if err := s.repo.DeleteLogsBefore(ctx, cutoff); err != nil {
					slog.Error("MCP log cleanup failed", "error", err)
				}
			}
		}
	}()
}

// CheckToolPermission checks if the given principal is allowed to call the specified tool.
// Returns nil if allowed, or an error describing the denial.
// If no permissions are configured for the server, all access is allowed.
func (s *MCPService) CheckToolPermission(ctx context.Context, serverID int64, apiKeyID int64, teamID *int64, toolName string) error {
	// Try permission cache first (60s TTL)
	var perms []MCPServerPermission
	s.permCache.mu.RLock()
	if entry, ok := s.permCache.items[serverID]; ok && time.Now().Before(entry.expiresAt) {
		perms = entry.perms
	}
	s.permCache.mu.RUnlock()

	if perms == nil {
		var err error
		perms, err = s.repo.ListPermissions(ctx, serverID)
		if err != nil {
			return fmt.Errorf("permission check unavailable: %w", err)
		}
		s.permCache.mu.Lock()
		s.permCache.items[serverID] = &permCacheEntry{
			perms:     perms,
			expiresAt: time.Now().Add(10 * time.Second),
		}
		s.permCache.mu.Unlock()
	}

	if len(perms) == 0 {
		return nil // no permissions configured = open access
	}

	for _, perm := range perms {
		matched := false
		switch perm.PrincipalType {
		case "key":
			matched = perm.PrincipalID == apiKeyID
		case "team":
			if teamID != nil {
				matched = perm.PrincipalID == *teamID
			}
		}
		if !matched {
			continue
		}

		// Check deny list first
		if len(perm.DenyTools) > 0 {
			var denyList []string
			if err := json.Unmarshal(perm.DenyTools, &denyList); err != nil {
				slog.Warn("failed to parse deny_tools", "perm_id", perm.ID, "error", err)
				continue
			}
			for _, d := range denyList {
				if d == toolName || d == "*" {
					return fmt.Errorf("tool %q denied by permission rule", toolName)
				}
			}
		}

		// Check allow list
		if len(perm.AllowTools) > 0 {
			var allowList []string
			if err := json.Unmarshal(perm.AllowTools, &allowList); err != nil {
				slog.Warn("failed to parse allow_tools", "perm_id", perm.ID, "error", err)
				continue
			}
			for _, a := range allowList {
				if a == toolName || a == "*" {
					return nil
				}
			}
			return fmt.Errorf("tool %q not in allow list", toolName)
		}

		return nil // matched principal, no tool restrictions
	}

	return fmt.Errorf("no matching permission rule for this principal")
}
