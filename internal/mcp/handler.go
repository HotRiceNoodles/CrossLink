package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
)

const maxRequestBodySize = 10 << 20 // 10MB

type Handler struct {
	svc *MCPService
}

func NewHandler(svc *MCPService) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) HandleJSONRPC(c *gin.Context) {
	serverName := c.Param("server")
	if serverName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing server name"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodySize)
	var req JSONRPCRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON-RPC request"})
		return
	}

	// Check tool permissions for tools/call
	if req.Method == MethodToolsCall {
		if err := h.checkPermission(c, serverName, &req); err != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
	}

	start := time.Now()
	resp, err := h.svc.ForwardRequest(c.Request.Context(), serverName, &req)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		slog.Error("MCP forward failed", "server", serverName, "method", req.Method, "error", err)
		h.logToolCallAsync(c, serverName, &req, duration, 0, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream server error"})
		return
	}

	outputSize := 0
	if resp.Result != nil {
		if b, marshalErr := json.Marshal(resp.Result); marshalErr == nil {
			outputSize = len(b)
		}
	}

	h.logToolCallAsync(c, serverName, &req, duration, outputSize, nil)
	c.JSON(http.StatusOK, resp)
}

// logToolCallAsync writes a tool call log record in the background.
func (h *Handler) logToolCallAsync(c *gin.Context, serverName string, req *JSONRPCRequest, duration int64, outputSize int, fwdErr error) {
	if req.Method != MethodToolsList && req.Method != MethodToolsCall {
		return
	}

	toolLog := &MCPToolCallLog{
		ServerName: serverName,
		Method:     req.Method,
		Duration:   int(duration),
		OutputSize: outputSize,
		Status:     1,
	}

	if fwdErr != nil {
		toolLog.Status = 0
		toolLog.ErrorMsg = fwdErr.Error()
		if len(toolLog.ErrorMsg) > 500 {
			toolLog.ErrorMsg = toolLog.ErrorMsg[:500]
		}
	}

	if req.Params != nil {
		toolLog.InputSize = len(req.Params)
	}

	if req.Method == MethodToolsCall {
		var params struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(req.Params, &params); err == nil {
			toolLog.ToolName = params.Name
		}
	}

	if keyID, ok := c.Get("api_key_id"); ok {
		if id, ok := keyID.(int64); ok {
			toolLog.APIKeyID = id
		}
	}

	if srv, _, ok := h.svc.registry.Get(serverName); ok {
		toolLog.ServerID = srv.ID
	}

	toolLog.RequestID = c.GetHeader("X-Request-Id")

	h.svc.LogToolCall(context.Background(), toolLog)
}

func (h *Handler) HandleSSE(c *gin.Context) {
	serverName := c.Param("server")
	if serverName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing server name"})
		return
	}

	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodySize)
	// Accept a JSON-RPC request from query param or body
	var req JSONRPCRequest
	if raw := c.Query("request"); raw != "" {
		if len(raw) > 64*1024 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "request query param too large (max 64KB)"})
			return
		}
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request query param"})
			return
		}
	} else if err := c.ShouldBindJSON(&req); err != nil {
		// No body — respond with a connection-established event
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Status(http.StatusOK)
		c.Writer.Flush()
		return
	}

	// Check tool permissions for tools/call
	if req.Method == MethodToolsCall {
		if err := h.checkPermission(c, serverName, &req); err != nil {
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
			fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":\"permission denied\"}\n\n")
			c.Writer.Flush()
			return
		}
	}

	resp, err := h.svc.ForwardRequest(c.Request.Context(), serverName, &req)
	if err != nil {
		slog.Error("MCP forward failed", "server", serverName, "method", req.Method, "error", err)
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":\"upstream server error\"}\n\n")
		c.Writer.Flush()
		return
	}

	respBytes, _ := json.Marshal(resp)
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	fmt.Fprintf(c.Writer, "data: %s\n\n", respBytes)
	c.Writer.Flush()
}

func (h *Handler) checkPermission(c *gin.Context, serverName string, req *JSONRPCRequest) error {
	srv, _, ok := h.svc.registry.Get(serverName)
	if !ok {
		return nil // server not found will be caught by ForwardRequest
	}

	var toolName string
	var params struct {
		Name string `json:"name"`
	}
	if req.Params != nil && json.Unmarshal(req.Params, &params) == nil {
		toolName = params.Name
	}
	if toolName == "" {
		return fmt.Errorf("tools/call requires a tool name")
	}

	var apiKeyID int64
	var teamID *int64
	if keyID, ok := c.Get("api_key_id"); ok {
		if id, ok := keyID.(int64); ok {
			apiKeyID = id
		}
	}
	if key, ok := c.Get("api_key"); ok {
		if ak, ok := key.(*model.APIKey); ok && ak.TeamID != nil {
			teamID = ak.TeamID
		}
	}

	return h.svc.CheckToolPermission(c.Request.Context(), srv.ID, apiKeyID, teamID, toolName)
}
