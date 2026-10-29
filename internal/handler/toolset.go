package handler

import (
	"encoding/json"

	"github.com/crosslink/internal/domain"
)

// maxDeclaredTools caps Declared per observation so a single (possibly
// abusive) request cannot flood the collector queue's memory.
const maxDeclaredTools = 128

// toolsetInput carries what the synchronous ToolObservation builder needs.
// Mirrors contextAnalysisInput: built inside the usage-entry closure where
// the request identity is still in scope.
type toolsetInput struct {
	keyID    int64
	teamID   int64
	orgID    int64
	model    string
	cacheHit bool
	invoked  []string // streaming path: tool names accumulated from SSE deltas
}

// observeTools hands a built observation to the toolset observer. nil
// observer, no key identity, or nothing to observe (no declared tools and no
// invocations) is a no-op — the single hot-path entry point.
func observeTools(o domain.ToolObserver, obs domain.ToolObservation) {
	if o == nil || obs.APIKeyID <= 0 || (len(obs.Declared) == 0 && len(obs.Invoked) == 0) {
		return
	}
	o.Observe(obs)
}

// buildOpenAIToolObservation extracts declared tools from an OpenAI-format
// request and invoked names from a non-stream response. resp == nil means
// streaming: the accumulated names arrive via in.invoked. Cache replays skip
// invoked counting (the response was not a fresh emission).
func buildOpenAIToolObservation(in toolsetInput, req *domain.OpenAIRequest, resp *domain.OpenAIResponse) domain.ToolObservation {
	obs := domain.ToolObservation{
		APIKeyID: in.keyID,
		TeamID:   in.teamID,
		OrgID:    in.orgID,
		Source:   "proxy",
		Protocol: "openai",
		Model:    in.model,
	}
	if req != nil {
		for i, t := range req.Tools {
			if i >= maxDeclaredTools {
				break
			}
			if t.Function.Name == "" {
				continue
			}
			obs.Declared = append(obs.Declared, domain.DeclaredTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				SchemaRaw:   t.Function.Parameters,
			})
		}
	}
	if !in.cacheHit {
		if len(in.invoked) > 0 {
			obs.Invoked = append(obs.Invoked, in.invoked...)
		}
		if resp != nil && len(resp.Choices) > 0 {
			for _, tc := range resp.Choices[0].Message.ToolCalls {
				if tc.Function.Name != "" {
					obs.Invoked = append(obs.Invoked, tc.Function.Name)
				}
			}
		}
	}
	return obs
}

// buildAnthropicToolObservation extracts declared tools from an Anthropic
// /v1/messages request (tools arrive as raw JSON and are parsed here) and
// invoked names from a non-stream response's tool_use blocks.
func buildAnthropicToolObservation(in toolsetInput, req *domain.AnthropicRequest, resp *domain.AnthropicResponse) domain.ToolObservation {
	obs := domain.ToolObservation{
		APIKeyID: in.keyID,
		TeamID:   in.teamID,
		OrgID:    in.orgID,
		Source:   "proxy",
		Protocol: "anthropic",
		Model:    in.model,
	}
	if req != nil && len(req.Tools) > 0 {
		var tools []domain.AnthropicTool
		if json.Unmarshal(req.Tools, &tools) == nil {
			for i, t := range tools {
				if i >= maxDeclaredTools {
					break
				}
				if t.Name == "" {
					continue
				}
				obs.Declared = append(obs.Declared, domain.DeclaredTool{
					Name:        t.Name,
					Description: t.Description,
					SchemaRaw:   t.InputSchema,
				})
			}
		}
	}
	if !in.cacheHit {
		if len(in.invoked) > 0 {
			obs.Invoked = append(obs.Invoked, in.invoked...)
		}
		if resp != nil {
			for _, b := range resp.Content {
				if b.Type == "tool_use" && b.Name != "" {
					obs.Invoked = append(obs.Invoked, b.Name)
				}
			}
		}
	}
	return obs
}

// streamToolCallNames returns the tool names carried by one streamed OpenAI
// delta. Providers send the full name in the first fragment per index, so
// appending every non-empty name counts each call exactly once.
func streamToolCallNames(delta domain.OpenAIChunkDelta) []string {
	var names []string
	for _, tc := range delta.ToolCalls {
		if tc.Function.Name != "" {
			names = append(names, tc.Function.Name)
		}
	}
	return names
}
