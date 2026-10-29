package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

const maxRequestBodySize = 10 << 20 // 10MB

// maxMCPDeclaredTools caps Declared per tools/list observation so one
// mega-server (hundreds of tools) cannot flood the observer queue.
const maxMCPDeclaredTools = 128

type Handler struct {
	svc     *MCPService
	guard   *guardrail.GuardrailService
	toolObs domain.ToolObserver // agent toolset profiling seam (Pro overlay); nil = disabled
}

func NewHandler(svc *MCPService) *Handler {
	return &Handler{svc: svc}
}

// SetToolObserver injects the agent toolset profiling observer (commercial
// overlay; mirrors the chat/messages/responses handlers). nil = disabled.
func (h *Handler) SetToolObserver(o domain.ToolObserver) { h.toolObs = o }

// mcpListTool is one entry of a tools/list result (MCP standard shape).
type mcpListTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mcpToolName namespaces an MCP tool under its server: cross-server name
// collisions are real and the profile key is (api_key_id, tool_name).
func mcpToolName(server, tool string) string { return "mcp/" + server + "/" + tool }

// observeMCPCall feeds the agent toolset seam. tools/list responses declare
// the server's tool inventory (inputSchema → sha, changes surface as
// definition_changed events); tools/call attempts — permission-denied,
// guardrail-blocked, failed or successful alike — count as invocations
// (attempted capability is the evidence). resp == nil on non-forward exits.
func (h *Handler) observeMCPCall(c *gin.Context, serverName string, req *JSONRPCRequest, resp *JSONRPCResponse) {
	if h.toolObs == nil {
		return
	}
	obs := domain.ToolObservation{Source: "proxy", Protocol: "mcp"}
	if v, ok := c.Get("api_key"); ok {
		if ak, ok := v.(*model.APIKey); ok {
			obs.APIKeyID = ak.ID
			if ak.TeamID != nil {
				obs.TeamID = *ak.TeamID
			}
			if ak.OrgID != nil {
				obs.OrgID = *ak.OrgID
			}
		}
	}
	if obs.APIKeyID <= 0 {
		return // config-auth callers have no key identity to profile
	}
	switch req.Method {
	case MethodToolsList:
		if resp == nil || resp.Error != nil || resp.Result == nil {
			return
		}
		b, err := json.Marshal(resp.Result)
		if err != nil {
			return
		}
		var result struct {
			Tools []mcpListTool `json:"tools"`
		}
		if json.Unmarshal(b, &result) != nil {
			return
		}
		for i, t := range result.Tools {
			if i >= maxMCPDeclaredTools {
				break
			}
			if t.Name == "" {
				continue
			}
			obs.Declared = append(obs.Declared, domain.DeclaredTool{
				Name:        mcpToolName(serverName, t.Name),
				Description: t.Description,
				SchemaRaw:   t.InputSchema,
			})
		}
	case MethodToolsCall:
		var params struct {
			Name string `json:"name"`
		}
		if req.Params == nil || json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
			return
		}
		obs.Invoked = []string{mcpToolName(serverName, params.Name)}
	default:
		return
	}
	if len(obs.Declared) == 0 && len(obs.Invoked) == 0 {
		return
	}
	h.toolObs.Observe(obs)
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
			h.observeMCPCall(c, serverName, &req, nil) // toolset: denied attempt
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		// Content guardrail: inspect (and possibly mask) the call arguments
		// before anything is forwarded upstream.
		if gerr := h.checkToolCallParams(c, &req); gerr != nil {
			resp := JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: gerr}
			h.logToolCallAsync(c, serverName, &req, 0, 0, fmt.Errorf("guardrail: %s", gerr.Message))
			h.observeMCPCall(c, serverName, &req, nil) // toolset: blocked attempt
			c.JSON(http.StatusOK, resp)
			return
		}
	}

	start := time.Now()
	resp, err := h.svc.ForwardRequest(c.Request.Context(), serverName, &req)
	duration := time.Since(start).Milliseconds()

	if err != nil {
		slog.Error("MCP forward failed", "server", serverName, "method", req.Method, "error", err)
		h.logToolCallAsync(c, serverName, &req, duration, 0, err)
		h.observeMCPCall(c, serverName, &req, nil) // toolset: failed attempt
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream server error"})
		return
	}

	outputSize := 0
	if resp.Result != nil {
		if b, marshalErr := json.Marshal(resp.Result); marshalErr == nil {
			outputSize = len(b)
		}
	}

	// Content guardrail: inspect (and possibly mask) the tool result before
	// it is returned to the client.
	h.checkToolCallResult(c, &req, resp)

	h.logToolCallAsync(c, serverName, &req, duration, outputSize, nil)
	h.observeMCPCall(c, serverName, &req, resp) // toolset: declared (tools/list) / invoked (tools/call)
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
			h.observeMCPCall(c, serverName, &req, nil) // toolset: denied attempt
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
			fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":\"permission denied\"}\n\n")
			c.Writer.Flush()
			return
		}
		// Content guardrail: inspect (and possibly mask) the call arguments
		// before anything is forwarded upstream.
		if gerr := h.checkToolCallParams(c, &req); gerr != nil {
			if b, merr := json.Marshal(JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: gerr}); merr == nil {
				h.observeMCPCall(c, serverName, &req, nil) // toolset: blocked attempt
				c.Header("Content-Type", "text/event-stream")
				c.Status(http.StatusOK)
				fmt.Fprintf(c.Writer, "data: %s\n\n", b)
				c.Writer.Flush()
				return
			}
			c.Header("Content-Type", "text/event-stream")
			c.Status(http.StatusOK)
			fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":\"blocked by guardrail\"}\n\n")
			c.Writer.Flush()
			return
		}
	}

	resp, err := h.svc.ForwardRequest(c.Request.Context(), serverName, &req)
	if err != nil {
		slog.Error("MCP forward failed", "server", serverName, "method", req.Method, "error", err)
		h.observeMCPCall(c, serverName, &req, nil) // toolset: failed attempt
		c.Header("Content-Type", "text/event-stream")
		c.Status(http.StatusOK)
		fmt.Fprintf(c.Writer, "event: error\ndata: {\"error\":\"upstream server error\"}\n\n")
		c.Writer.Flush()
		return
	}

	// Content guardrail: inspect (and possibly mask) the tool result before
	// it is returned to the client.
	h.checkToolCallResult(c, &req, resp)

	h.observeMCPCall(c, serverName, &req, resp) // toolset: declared (tools/list) / invoked (tools/call)

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
