package middleware

import (
	"encoding/json"

	"github.com/crosslink/internal/domain"
	"github.com/gin-gonic/gin"
)

// Guardrail-blocked toolset observation (Declared-only). Middleware cannot
// reuse the handler package's builders — handler imports middleware for key
// identity, so the dependency only flows one way — so the wire shapes are
// parsed here from the already-buffered body bytes.

// maxBlockedDeclaredTools caps Declared per blocked-request observation,
// mirroring the handler builders' cap so a single request cannot flood the
// collector queue.
const maxBlockedDeclaredTools = 128

// blockedToolProtocol maps the matched route to the tools[] dialect. The
// /v1/responses tools share the OpenAI function shape, so everything but
// /v1/messages is "openai".
func blockedToolProtocol(c *gin.Context) string {
	if c.FullPath() == "/v1/messages" {
		return "anthropic"
	}
	return "openai"
}

// rawBlockedTool tolerates the two OpenAI-dialect tool layouts: chat style
// nests the definition under function, native Responses style flattens it.
type rawBlockedTool struct {
	Type        string                  `json:"type"`
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	Parameters  json.RawMessage         `json:"parameters"`
	Function    *rawBlockedToolFunction `json:"function"`
}

type rawBlockedToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// parseDeclaredToolsFromRaw extracts declared tool definitions from a raw
// request body. Best-effort: malformed or tool-less bodies yield nil.
func parseDeclaredToolsFromRaw(protocol string, body []byte) []domain.DeclaredTool {
	switch protocol {
	case "anthropic":
		var req struct {
			Tools []domain.AnthropicTool `json:"tools"`
		}
		if json.Unmarshal(body, &req) != nil {
			return nil
		}
		var declared []domain.DeclaredTool
		for i, t := range req.Tools {
			if i >= maxBlockedDeclaredTools {
				break
			}
			if t.Name == "" {
				continue
			}
			declared = append(declared, domain.DeclaredTool{
				Name:        t.Name,
				Description: t.Description,
				SchemaRaw:   t.InputSchema,
			})
		}
		return declared
	default: // openai: /v1/chat/completions + /v1/responses
		var req struct {
			Tools []rawBlockedTool `json:"tools"`
		}
		if json.Unmarshal(body, &req) != nil {
			return nil
		}
		var declared []domain.DeclaredTool
		for i, t := range req.Tools {
			if i >= maxBlockedDeclaredTools {
				break
			}
			name, desc, params := t.Name, t.Description, t.Parameters
			if name == "" && t.Function != nil {
				name, desc, params = t.Function.Name, t.Function.Description, t.Function.Parameters
			}
			if name == "" || (t.Type != "function" && t.Type != "") {
				continue
			}
			declared = append(declared, domain.DeclaredTool{
				Name:        name,
				Description: desc,
				SchemaRaw:   params,
			})
		}
		return declared
	}
}
