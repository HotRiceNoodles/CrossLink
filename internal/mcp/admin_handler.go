package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/crosslink/internal/admin"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type AdminHandler struct {
	svc      *MCPService
	encStore Encrypter
	auditSvc *service.AuditService // set by commercial build; nil in Community
}

func NewAdminHandler(svc *MCPService, encStore Encrypter) *AdminHandler {
	return &AdminHandler{svc: svc, encStore: encStore}
}

// SetEncStore allows late binding of the encryption store.
func (h *AdminHandler) SetEncStore(encStore Encrypter) {
	h.encStore = encStore
}

// SetAuditLogger allows late binding of the audit service (commercial build
// only; Community never calls it, keeping admin writes audit-free there).
func (h *AdminHandler) SetAuditLogger(auditSvc *service.AuditService) {
	h.auditSvc = auditSvc
}

type createServerInput struct {
	Name          string          `json:"name" binding:"required"`
	DisplayName   string          `json:"display_name"`
	Description   string          `json:"description"`
	TransportType string          `json:"transport_type" binding:"required"`
	URL           string          `json:"url"`
	AuthType      string          `json:"auth_type"`
	AuthConfig    json.RawMessage `json:"auth_config"`
	CustomHeaders json.RawMessage `json:"custom_headers"`
}

func (h *AdminHandler) Create(c *gin.Context) {
	var input createServerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:create", "mcp_server", "", input.Name, service.AuditDetail(map[string]any{"reason": "invalid_request", "error": err.Error()}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := admin.GetOrgID(c)

	srv := &MCPServer{
		Name:          input.Name,
		DisplayName:   input.DisplayName,
		Description:   input.Description,
		TransportType: input.TransportType,
		URL:           input.URL,
		AuthType:      input.AuthType,
		Enabled:       true,
		Status:        1,
	}
	if orgID != 0 {
		srv.OrgID = &orgID
	}
	if srv.AuthType == "" {
		srv.AuthType = "none"
	}

	// Validate URL for HTTP/SSE transport
	if (srv.TransportType == "http" || srv.TransportType == "sse") && srv.URL != "" {
		if err := validateServerURL(srv.URL); err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "mcp_server:create", "mcp_server", "", srv.Name, service.AuditDetail(map[string]any{"reason": "url_rejected", "url": srv.URL, "error": err.Error()}))
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	// Encrypt sensitive fields in AuthConfig before storage
	if len(input.AuthConfig) > 0 {
		encrypted, err := h.encryptJSONSensitiveFields(input.AuthConfig)
		if err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "mcp_server:create", "mcp_server", "", srv.Name, service.AuditDetail(map[string]any{"reason": "encrypt_failed", "error": err.Error()}))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt auth config failed"})
			return
		}
		srv.AuthConfig = encrypted
	}
	if len(input.CustomHeaders) > 0 {
		srv.CustomHeaders = datatypes.JSON(input.CustomHeaders)
	}

	if err := h.svc.CreateServer(c.Request.Context(), srv); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:create", "mcp_server", "", srv.Name, service.AuditDetail(map[string]any{"reason": "create_failed", "error": err.Error()}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_server:create", "mcp_server", strconv.FormatInt(srv.ID, 10), srv.Name, service.AuditDetail(map[string]any{"transport_type": srv.TransportType, "auth_type": srv.AuthType, "enabled": srv.Enabled}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": srv})
}

func (h *AdminHandler) List(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	servers, err := h.svc.ListServers(c.Request.Context(), orgID)
	if err != nil {
		slog.Error("list MCP servers", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	for i := range servers {
		sanitizeServer(&servers[i])
	}
	c.JSON(http.StatusOK, gin.H{"data": servers})
}

func (h *AdminHandler) Get(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	srv, err := h.svc.GetServer(c.Request.Context(), orgID, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}
	sanitizeServer(srv)
	c.JSON(http.StatusOK, gin.H{"data": srv})
}

type updateServerInput struct {
	DisplayName   *string         `json:"display_name"`
	Description   *string         `json:"description"`
	URL           *string         `json:"url"`
	Enabled       *bool           `json:"enabled"`
	TransportType *string         `json:"transport_type"`
	AuthType      *string         `json:"auth_type"`
	AuthConfig    json.RawMessage `json:"auth_config"`
	CustomHeaders json.RawMessage `json:"custom_headers"`
}

func (h *AdminHandler) Update(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	srv, err := h.svc.GetServer(c.Request.Context(), orgID, id)
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "not_found"}))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}

	var input updateServerInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "invalid_request", "error": err.Error()}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if input.DisplayName != nil {
		srv.DisplayName = *input.DisplayName
	}
	if input.Description != nil {
		srv.Description = *input.Description
	}
	if input.Enabled != nil {
		srv.Enabled = *input.Enabled
	}
	if input.TransportType != nil {
		srv.TransportType = *input.TransportType
	}
	if input.URL != nil {
		if *input.URL == "" && (srv.TransportType == "http" || srv.TransportType == "sse") {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "url_required"}))
			}
			c.JSON(http.StatusBadRequest, gin.H{"error": "url is required for HTTP/SSE transport"})
			return
		}
		if *input.URL != "" && (srv.TransportType == "http" || srv.TransportType == "sse") {
			if err := validateServerURL(*input.URL); err != nil {
				if h.auditSvc != nil {
					h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "url_rejected", "url": *input.URL, "error": err.Error()}))
				}
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		srv.URL = *input.URL
	}
	// After all field assignments, validate http/sse requires a URL
	if (srv.TransportType == "http" || srv.TransportType == "sse") && srv.URL == "" {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "url_required"}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "url is required for HTTP/SSE transport"})
		return
	}
	if input.AuthType != nil {
		srv.AuthType = *input.AuthType
	}
	if len(input.AuthConfig) > 0 {
		encrypted, err := h.encryptJSONSensitiveFields(input.AuthConfig)
		if err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "encrypt_failed", "error": err.Error()}))
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt auth config failed"})
			return
		}
		srv.AuthConfig = encrypted
	}
	if len(input.CustomHeaders) > 0 {
		srv.CustomHeaders = datatypes.JSON(input.CustomHeaders)
	}

	if err := h.svc.UpdateServer(c.Request.Context(), srv); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"reason": "update_failed", "error": err.Error()}))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_server:update", "mcp_server", c.Param("id"), srv.Name, service.AuditDetail(map[string]any{"transport_type": srv.TransportType, "auth_type": srv.AuthType, "enabled": srv.Enabled}))
	}
	c.JSON(http.StatusOK, gin.H{"data": srv})
}

func (h *AdminHandler) Delete(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	if err := h.svc.DeleteServer(c.Request.Context(), orgID, id); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:delete", "mcp_server", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "not_found", "error": err.Error()}))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_server:delete", "mcp_server", c.Param("id"), "", nil)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func (h *AdminHandler) Test(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	if err := h.svc.TestServer(c.Request.Context(), orgID, id); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_server:test", "mcp_server", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "test_failed", "error": err.Error()}))
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error(), "healthy": false})
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_server:test", "mcp_server", c.Param("id"), "", nil)
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"healthy": true}})
}

func (h *AdminHandler) GetTools(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	srv, err := h.svc.GetServer(c.Request.Context(), orgID, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}
	tools, err := h.svc.GetServerTools(c.Request.Context(), srv.Name)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": tools})
}

func parseIDParam(c *gin.Context) int64 {
	n, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return n
}

// encryptJSONSensitiveFields encrypts known-sensitive keys in a JSON object.
func (h *AdminHandler) encryptJSONSensitiveFields(raw json.RawMessage) (datatypes.JSON, error) {
	if h.encStore == nil || len(raw) == 0 {
		return datatypes.JSON(raw), nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return datatypes.JSON(raw), nil
	}
	changed := false
	for k, v := range m {
		if secret.IsSensitiveField(k) {
			if s, ok := v.(string); ok && s != "" && !secret.IsReference(s) && !h.encStore.IsEncrypted(s) {
				enc, err := h.encStore.Encrypt(s)
				if err != nil {
					return nil, err
				}
				m[k] = enc
				changed = true
			}
		}
	}
	if !changed {
		return datatypes.JSON(raw), nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(out), nil
}

func (h *AdminHandler) ListPermissions(c *gin.Context) {
	id := parseIDParam(c)
	perms, err := h.svc.repo.ListPermissions(c.Request.Context(), id)
	if err != nil {
		slog.Error("list MCP permissions", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": perms})
}

type createPermissionInput struct {
	PrincipalType string          `json:"principal_type" binding:"required"`
	PrincipalID   int64           `json:"principal_id" binding:"required"`
	AllowTools    json.RawMessage `json:"allow_tools"`
	DenyTools     json.RawMessage `json:"deny_tools"`
}

func (h *AdminHandler) CreatePermission(c *gin.Context) {
	serverID := parseIDParam(c)
	var input createPermissionInput
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_permission:create", "mcp_permission", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "invalid_request", "error": err.Error()}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	perm := &MCPServerPermission{
		ServerID:      serverID,
		PrincipalType: input.PrincipalType,
		PrincipalID:   input.PrincipalID,
	}
	if len(input.AllowTools) > 0 {
		perm.AllowTools = datatypes.JSON(input.AllowTools)
	}
	if len(input.DenyTools) > 0 {
		perm.DenyTools = datatypes.JSON(input.DenyTools)
	}

	if err := h.svc.repo.CreatePermission(c.Request.Context(), perm); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_permission:create", "mcp_permission", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "create_failed", "principal_type": input.PrincipalType, "principal_id": input.PrincipalID, "error": err.Error()}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.svc.InvalidatePermCache(serverID)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_permission:create", "mcp_permission", strconv.FormatInt(perm.ID, 10), "", service.AuditDetail(map[string]any{"server_id": serverID, "principal_type": perm.PrincipalType, "principal_id": perm.PrincipalID}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": perm})
}

func (h *AdminHandler) DeletePermission(c *gin.Context) {
	pid, _ := strconv.ParseInt(c.Param("pid"), 10, 64)
	if pid == 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_permission:delete", "mcp_permission", c.Param("pid"), "", service.AuditDetail(map[string]any{"reason": "invalid_id"}))
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid permission id"})
		return
	}
	if err := h.svc.repo.DeletePermission(c.Request.Context(), pid); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "mcp_permission:delete", "mcp_permission", c.Param("pid"), "", service.AuditDetail(map[string]any{"reason": "not_found", "error": err.Error()}))
		}
		c.JSON(http.StatusNotFound, gin.H{"error": "permission not found"})
		return
	}
	serverID := parseIDParam(c)
	h.svc.InvalidatePermCache(serverID)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "mcp_permission:delete", "mcp_permission", c.Param("pid"), "", service.AuditDetail(map[string]any{"server_id": serverID}))
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"deleted": true}})
}

func (h *AdminHandler) ListLogs(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var serverID int64
	if id > 0 {
		srv, err := h.svc.GetServer(c.Request.Context(), orgID, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
			return
		}
		serverID = srv.ID
	}

	logs, total, err := h.svc.repo.ListToolCallLogs(c.Request.Context(), orgID, serverID, page, pageSize)
	if err != nil {
		slog.Error("list MCP logs", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       logs,
		"pagination": gin.H{"total": total, "page": page, "page_size": pageSize},
	})
}

func (h *AdminHandler) GetStats(c *gin.Context) {
	orgID := admin.GetOrgID(c)
	id := parseIDParam(c)
	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days < 1 {
		days = 7
	}
	if days > 90 {
		days = 90
	}

	var serverID int64
	if id > 0 {
		srv, err := h.svc.GetServer(c.Request.Context(), orgID, id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
			return
		}
		serverID = srv.ID
	}

	stats, err := h.svc.repo.GetToolCallStats(c.Request.Context(), orgID, serverID, days)
	if err != nil {
		slog.Error("get MCP stats", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	topTools, err := h.svc.repo.GetTopTools(c.Request.Context(), orgID, serverID, days, 10)
	if err != nil {
		slog.Error("get MCP top tools", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	callsByDay, err := h.svc.repo.GetCallsByDay(c.Request.Context(), orgID, serverID, days)
	if err != nil {
		slog.Error("get MCP calls by day", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"stats":        stats,
		"top_tools":    topTools,
		"calls_by_day": callsByDay,
		"days":         days,
	})
}
func validateServerURL(rawURL string) error {
	// Delegate to the shared SSRF validator: enforces http/https scheme, resolves
	// the hostname via DNS, and rejects loopback/private/link-local/cloud-metadata
	// targets (so "localhost" and domains that resolve to internal IPs are blocked).
	// Operator-allowlisted internal CIDRs (gateway.internal_allow_cidrs) are honored,
	// consistent with the outbound SSRF dialer.
	return guardrail.ValidateURLSafety(rawURL)
}

func sanitizeServer(srv *MCPServer) {
	srv.AuthConfig = nil
	srv.CustomHeaders = nil
}
