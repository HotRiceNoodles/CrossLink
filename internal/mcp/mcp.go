package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// JSON-RPC 2.0 message types aligned with MCP specification.

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type JSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// MCP method constants.
const (
	MethodInitialize    = "initialize"
	MethodToolsList     = "tools/list"
	MethodToolsCall     = "tools/call"
	MethodResourcesList = "resources/list"
	MethodResourcesRead = "resources/read"
	MethodPromptsList   = "prompts/list"
	MethodPromptsGet    = "prompts/get"
	MethodPing          = "ping"
	MethodCompletion    = "completion/complete"

	// Notifications (no ID, client to server or server to client)
	NotificationInitialized      = "notifications/initialized"
	NotificationToolsListChanged  = "notifications/tools/list_changed"
	NotificationCancelled         = "notifications/cancelled"
)

// serverNameRegex validates MCP server names used as URL path segments.
// Aligned with LiteLLM SEP-986: lowercase alphanumeric, hyphens, underscores.
var serverNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,62}[a-z0-9]$`)

func ValidateServerName(name string) error {
	if !serverNameRegex.MatchString(name) {
		return fmt.Errorf("invalid server name %q: must be 3-64 chars, lowercase alphanumeric with hyphens/underscores, no leading/trailing hyphens", name)
	}
	return nil
}
