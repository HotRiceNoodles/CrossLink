package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/translator"
	"gorm.io/datatypes"
)

const maxGuardrailBodySize = 10 << 20

func GuardrailsRequest(svc *guardrail.GuardrailService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || !svc.IsEnabled() {
			c.Next()
			return
		}

		if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") &&
			(strings.HasPrefix(c.Request.URL.Path, "/v1/audio/") ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/transcribe") ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/translate")) {
			c.Next()
			return
		}

		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Skip admin routes (except playground which proxies LLM calls)
		if strings.HasPrefix(c.Request.URL.Path, "/admin/api/") && !strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground") {
			c.Next()
			return
		}

		bodyBytes := GetBodyBytes(c)
		if bodyBytes == nil {
			var err error
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, int64(maxGuardrailBodySize)))
			if err != nil {
				slog.Error("guardrail: failed to read request body", "error", err)
				c.JSON(http.StatusBadRequest, gin.H{
					"error": gin.H{"type": "guardrail_error", "message": "failed to read request body"},
				})
				c.Abort()
				return
			}
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		var probe struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(bodyBytes, &probe) != nil {
			c.Next()
			return
		}

		content := extractContentFromBody(bodyBytes, c.Request.URL.Path)
		if content == "" {
			c.Next()
			return
		}

		var apiKeyID, teamID int64
		if key := GetAPIKeyFromContext(c); key != nil {
			apiKeyID = key.ID
			if key.TeamID != nil {
				teamID = *key.TeamID
			}
		}

		var orgID int64
		if v := c.GetInt64("org_id"); v != 0 {
			orgID = v
		}

		checkCtx := &guardrail.CheckContext{Metadata: make(map[string]string)}
		if apiKeyID > 0 {
			checkCtx.Metadata["api_key_id"] = fmt.Sprintf("%d", apiKeyID)
		}
		ctx := context.WithValue(c.Request.Context(), guardrail.CtxKeyCheck, checkCtx)
		ctx = context.WithValue(ctx, guardrail.CtxKeyUserAgent, c.GetHeader("User-Agent"))
		ctx = context.WithValue(ctx, guardrail.CtxKeyHeaders, c.Request.Header)
		c.Request = c.Request.WithContext(ctx)

		guardCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

			result, err := svc.Check(guardCtx, &guardrail.CheckRequest{
			Content:   content,
			Direction: guardrail.DirectionRequest,
			Model:     probe.Model,
			APIKeyID:  apiKeyID,
			TeamID:    teamID,
			OrgID:     orgID,
		})
		if err != nil {
			if svc.IsFailOpen() {
				slog.Warn("guardrail check failed, failing open", "error", err)
				c.Next()
				return
			}
			slog.Error("guardrail: request check failed", "error", err)
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": gin.H{"type": "guardrail_error", "message": "guardrail service unavailable"},
			})
			c.Abort()
			return
		}

		// Extract agent_type from CheckContext for usage logging
		if checkCtx.AgentType != "" {
			c.Set("agent_type", checkCtx.AgentType)
		}

		if result == nil || !result.Blocked {
			if result != nil && (result.Action == "log" || result.Action == "mask") {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", result.RuleName)
				setSecurityEvent(c, result)
				if result.Action == "mask" && result.MaskedContent != "" {
					maskedBody, ok := replaceContentInBody(bodyBytes, result.MaskedContent, c.Request.URL.Path)
					if ok {
						c.Request.Body = io.NopCloser(bytes.NewReader(maskedBody))
					}
				}
			}
			c.Next()
			return
		}

		switch result.Action {
		case "block":
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "log":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result)
		case "mask":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result)
			if result.MaskedContent != "" {
				maskedBody, ok := replaceContentInBody(bodyBytes, result.MaskedContent, c.Request.URL.Path)
				if ok {
					c.Request.Body = io.NopCloser(bytes.NewReader(maskedBody))
				}
			}
		}

		c.Next()
	}
}

func extractContentFromBody(body []byte, path string) string {
	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return extractOpenAIMessages(body)
	case strings.HasSuffix(path, "/v1/messages"):
		return extractAnthropicMessages(body)
	case strings.HasSuffix(path, "/v1/embeddings"):
		return extractEmbeddingsInput(body)
	case strings.HasSuffix(path, "/v1/images/generations"):
		return extractImagePrompt(body)
	case strings.HasSuffix(path, "/v1/videos"):
		return extractPrompt(body)
	case strings.HasSuffix(path, "/v1/responses"):
		return extractResponsesInput(body)
	case strings.HasPrefix(path, "/admin/api/playground/"):
		if strings.HasSuffix(path, "/chat") || strings.HasSuffix(path, "/stream") {
			return extractOpenAIMessages(body)
		}
		return extractPrompt(body)
	}
	return ""
}

func extractPrompt(body []byte) string {
	var v struct {
		Prompt string `json:"prompt"`
		Input  string `json:"input"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if v.Prompt != "" {
		return v.Prompt
	}
	return v.Input
}

func replacePrompt(body []byte, maskedContent string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	if _, ok := m["prompt"]; ok {
		m["prompt"], _ = json.Marshal(maskedContent)
		result, err := json.Marshal(m)
		return result, err == nil
	}
	if _, ok := m["input"]; ok {
		m["input"], _ = json.Marshal(maskedContent)
		result, err := json.Marshal(m)
		return result, err == nil
	}
	return nil, false
}

func extractImagePrompt(body []byte) string {
	var req struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	return req.Prompt
}

// extractResponsesInput extracts scannable text from a Responses API body:
// instructions + input (polymorphic: string, or array of message/function_call_output items).
func extractResponsesInput(body []byte) string {
	var req struct {
		Input         json.RawMessage `json:"input"`
		Instructions  string          `json:"instructions"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}
	var parts []string
	if req.Instructions != "" {
		parts = append(parts, req.Instructions)
	}
	if len(req.Input) > 0 {
		var s string
		if json.Unmarshal(req.Input, &s) == nil && req.Input[0] == '"' {
			parts = append(parts, s)
		} else {
			var items []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
				Output  string          `json:"output"`
			}
			if json.Unmarshal(req.Input, &items) == nil {
				for _, it := range items {
					if it.Type == "message" && len(it.Content) > 0 {
						parts = append(parts, domain.ContentText(it.Content))
					}
					if it.Type == "function_call_output" && it.Output != "" {
						parts = append(parts, it.Output)
					}
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// replaceResponsesInput masks instructions and (string) input in a Responses body.
// Array input masking is best-effort and not applied (returns false to fall through).
func replaceResponsesInput(body []byte, maskedContent string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	changed := false
	if raw, ok := m["input"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil && len(raw) > 0 && raw[0] == '"' {
			m["input"], _ = json.Marshal(maskedContent)
			changed = true
		}
	}
	if _, ok := m["instructions"]; ok {
		m["instructions"], _ = json.Marshal(maskedContent)
		changed = true
	}
	if !changed {
		return nil, false
	}
	result, err := json.Marshal(m)
	return result, err == nil
}


// replaceContentInBody replaces text content in the request body with masked content.
func replaceContentInBody(body []byte, maskedContent string, path string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}

	switch {
	case strings.HasSuffix(path, "/v1/chat/completions"):
		return replaceOpenAIMessages(body, maskedContent)
	case strings.HasSuffix(path, "/v1/responses"):
		return replaceResponsesInput(body, maskedContent)
	case strings.HasSuffix(path, "/v1/images/generations"):
		return replacePrompt(body, maskedContent)
	case strings.HasSuffix(path, "/v1/videos"):
		return replacePrompt(body, maskedContent)
	case strings.HasPrefix(path, "/admin/api/playground/"):
		if strings.HasSuffix(path, "/chat") || strings.HasSuffix(path, "/stream") {
			return replaceOpenAIMessages(body, maskedContent)
		}
		return replacePrompt(body, maskedContent)
	case strings.HasSuffix(path, "/v1/messages"):
		return replaceAnthropicMessages(body, maskedContent)
	case strings.HasSuffix(path, "/v1/embeddings"):
		if raw, ok := m["input"]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				m["input"], _ = json.Marshal(maskedContent)
			} else {
				var arr []string
				if json.Unmarshal(raw, &arr) == nil {
					for i := range arr {
						arr[i] = maskedContent
					}
					m["input"], _ = json.Marshal(arr)
				}
			}
			result, err := json.Marshal(m)
			return result, err == nil
		}
	}
	return nil, false
}

func replaceOpenAIMessages(body []byte, maskedContent string) ([]byte, bool) {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	raw, ok := req["messages"]
	if !ok {
		return nil, false
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(raw, &messages) != nil {
		return nil, false
	}
	textContent, _ := json.Marshal(maskedContent)
	for i := range messages {
		if role, ok := messages[i]["role"]; ok && string(role) == `"user"` {
			messages[i]["content"] = textContent
			delete(messages[i], "reasoning_content")
		}
	}
	updated, _ := json.Marshal(messages)
	req["messages"] = updated
	result, err := json.Marshal(req)
	return result, err == nil
}
func replaceAnthropicMessages(body []byte, maskedContent string) ([]byte, bool) {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	if raw, ok := req["messages"]; ok {
		var messages []map[string]json.RawMessage
		if json.Unmarshal(raw, &messages) == nil {
			textContent, _ := json.Marshal(maskedContent)
			for i := range messages {
				if role, ok := messages[i]["role"]; ok && string(role) == `"user"` {
					messages[i]["content"] = textContent
				}
			}
			updated, _ := json.Marshal(messages)
			req["messages"] = updated
		}
	}
	result, err := json.Marshal(req)
	return result, err == nil
}

func extractOpenAIMessages(body []byte) string {
	var req struct {
		Messages []domain.OpenAIMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}

	var parts []string
	for _, msg := range req.Messages {
		text := domain.ContentText(msg.Content)
		if text != "" {
			parts = append(parts, text)
		}
		if msg.ReasoningContent != "" {
			parts = append(parts, msg.ReasoningContent)
		}
	}
	return strings.Join(parts, "\n")
}

func extractAnthropicMessages(body []byte) string {
	var req struct {
		Messages []domain.AnthropicMessage `json:"messages"`
		System   json.RawMessage           `json:"system"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}

	var parts []string
	if req.System != nil {
		if sysText := translator.ExtractContentText(req.System); sysText != "" {
			parts = append(parts, sysText)
		}
	}
	for _, msg := range req.Messages {
		text := translator.ExtractContentText(msg.Content)
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func extractEmbeddingsInput(body []byte) string {
	var req struct {
		Input any `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return ""
	}

	switch v := req.Input.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func setSecurityEvent(c *gin.Context, result *guardrail.CheckResponse) {
	event := map[string]string{
		"rule":   result.RuleName,
		"action": result.Action,
		"reason": result.Reason,
	}
	if data, err := json.Marshal([]map[string]string{event}); err == nil {
		c.Set("security_events", datatypes.JSON(data))
	}
}

func writeGuardrailError(c *gin.Context, result *guardrail.CheckResponse, path string) {
	msg := "blocked by guardrail"

	if strings.HasSuffix(path, "/v1/messages") {
		c.JSON(http.StatusBadRequest, gin.H{
			"type":  "error",
			"error": gin.H{"type": "guardrail_blocked", "message": msg},
		})
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{"type": "guardrail_blocked", "message": msg},
	})
}
