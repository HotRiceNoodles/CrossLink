package translator

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/crosslink/internal/domain"
)

// generateResponseID synthesizes a Responses API response id (resp_ prefix).
func generateResponseID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "resp_" + hex.EncodeToString(b)
}

// supportsResponses parses the model-level ExtraConfig flag.
// Exported so handlers can determine the 3A/3B dispatch without a struct change.
func SupportsResponses(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var flag struct {
		SupportsResponses bool `json:"supports_responses"`
	}
	_ = json.Unmarshal(raw, &flag)
	return flag.SupportsResponses
}

// ResponsesToOpenAI translates a Responses API request into the internal OpenAI
// request (3B path). Handles polymorphic input (string or []item) with full
// item-type mapping: message / function_call / function_call_output / reasoning.
func ResponsesToOpenAI(req *domain.ResponsesRequest) (*domain.OpenAIRequest, error) {
	out := &domain.OpenAIRequest{
		Model:       req.Model,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		User:        req.User,
	}
	if req.MaxOutputTokens != nil {
		out.MaxTokens = req.MaxOutputTokens
	}

	// instructions → leading system message
	if req.Instructions != "" {
		out.Messages = append(out.Messages, domain.OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}

	msgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		return nil, err
	}
	out.Messages = append(out.Messages, msgs...)

	// tools (function) with strict passthrough
	if len(req.Tools) > 0 {
		out.Tools = make([]domain.OpenAITool, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "function" && t.Type != "" {
				continue
			}
			out.Tools = append(out.Tools, domain.OpenAITool{
				Type: "function",
				Function: domain.OpenAIFunctionDef{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
					Strict:      t.Function.Strict,
				},
			})
		}
	}
	if req.ToolChoice != nil {
		out.ToolChoice = req.ToolChoice
	}

	// reasoning.effort → reasoning_effort
	if len(req.Reasoning) > 0 {
		var r struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(req.Reasoning, &r) == nil && r.Effort != "" {
			out.ReasoningEffort = r.Effort
		}
	}

	return out, nil
}

// responsesInputToMessages maps polymorphic Responses input to OpenAI messages.
func responsesInputToMessages(raw json.RawMessage) ([]domain.OpenAIMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Case 1: input is a plain string.
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		// Only succeeds when raw is a JSON string.
		if len(raw) > 0 && raw[0] == '"' {
			return []domain.OpenAIMessage{{Role: "user", Content: asString}}, nil
		}
	}

	// Case 2: input is an array of items.
	var items []domain.ResponsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("responses input must be string or array: %w", err)
	}

	var msgs []domain.OpenAIMessage
	for _, it := range items {
		switch it.Type {
		case "message":
			role := it.Role
			if role == "" {
				role = "user"
			}
			msgs = append(msgs, domain.OpenAIMessage{
				Role:    role,
				Content: rawContentToAny(it.Content),
			})
		case "function_call":
			msgs = append(msgs, domain.OpenAIMessage{
				Role: "assistant",
				ToolCalls: []domain.OpenAIToolCall{{
					ID:   it.CallID,
					Type: "function",
					Function: domain.OpenAIFunctionCall{
						Name:      it.Name,
						Arguments: it.Arguments,
					},
				}},
			})
		case "function_call_output":
			msgs = append(msgs, domain.OpenAIMessage{
				Role:       "tool",
				ToolCallID: it.CallID,
				Content:    it.Output,
			})
		case "reasoning":
			// dropped (best-effort; OpenAI has no standard reasoning input message)
		}
	}
	return msgs, nil
}

// rawContentToAny returns a value suitable for OpenAIMessage.Content from a
// Responses message content field (string or array of parts). nil → string "".
func rawContentToAny(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && len(raw) > 0 && raw[0] == '"' {
		return s
	}
	// Array of parts: extract text parts joined, matching ContentText semantics.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		return domain.ContentText(raw)
	}
	return string(raw)
}

var finishReasonToResponsesStatus = map[string]string{
	"stop":           "completed",
	"tool_calls":     "completed",
	"length":         "incomplete",
	"content_filter": "incomplete",
}

// OpenAIToResponses translates an internal OpenAI response into a Responses API
// response (3B reverse path). choices[0] only; text → message item,
// tool_calls → function_call items (after the message item).
func OpenAIToResponses(resp *domain.OpenAIResponse, model string) *domain.ResponsesResponse {
	out := &domain.ResponsesResponse{
		ID:     generateResponseID(),
		Object: "response",
		Status: "completed",
		Model:  model,
		Usage: ResponsesUsageFromOpenAI(resp.Usage),
	}
	if len(resp.Choices) == 0 {
		return out
	}
	ch := resp.Choices[0]
	if status, ok := finishReasonToResponsesStatus[ch.FinishReason]; ok {
		out.Status = status
	}

	// Text message item (only if there is text content).
	if text := domain.ContentText(ch.Message.Content); text != "" {
		out.Output = append(out.Output, domain.ResponsesOutputItem{
			Type:   "message",
			ID:     generateMessageID(),
			Role:   "assistant",
			Status: "completed",
			Content: []domain.ResponsesContentPart{{
				Type: "output_text",
				Text: text,
			}},
		})
	}
	// Function call items (one per tool call).
	for _, tc := range ch.Message.ToolCalls {
		out.Output = append(out.Output, domain.ResponsesOutputItem{
			Type:      "function_call",
			ID:        generateMessageID(),
			Status:    "completed",
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out
}

// ResponsesUsageFromOpenAI maps OpenAI usage fields to Responses usage.
func ResponsesUsageFromOpenAI(u domain.OpenAIUsage) domain.ResponsesUsage {
	ru := domain.ResponsesUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		ru.InputTokensDetails = &domain.ResponsesInputTokensDetails{CachedTokens: u.PromptTokensDetails.CachedTokens}
	}
	if u.CompletionTokensDetails != nil {
		ru.OutputTokensDetails = &domain.ResponsesOutputTokensDetails{ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens}
	}
	return ru
}
