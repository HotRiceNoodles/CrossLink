package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
	"gorm.io/datatypes"
)

const maxContentLen = 65536

func truncateContent(s string) string {
	if utf8.RuneCountInString(s) <= maxContentLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxContentLen])
}

type AnthropicHandler struct {
	svc         *service.GatewayService
	usageSvc    *service.UsageService
	idemCache   *service.IdempotencyCache
	guardrailSvc *guardrail.GuardrailService
}

func NewAnthropicHandler(svc *service.GatewayService, usageSvc *service.UsageService, idemCache *service.IdempotencyCache, guardrailSvc *guardrail.GuardrailService) *AnthropicHandler {
	return &AnthropicHandler{svc: svc, usageSvc: usageSvc, idemCache: idemCache, guardrailSvc: guardrailSvc}
}

func (h *AnthropicHandler) logFailure(c *gin.Context, model string, start time.Time, gatewayErr error) {
	var keyID int64
	var teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		keyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}
	var currency string
	var providerID int64
	var routeErr *service.RouteError
	if errors.As(gatewayErr, &routeErr) && routeErr.Route != nil {
		currency = routeErr.Route.Currency
		providerID = routeErr.Route.ProviderRow.ID
	}
	var fallbackCount int
	var retryCount int
	if routeErr != nil {
		fallbackCount = routeErr.FallbackCount
		retryCount = routeErr.RetryCount
	}
	c.Set("usage_logged", true)
	submitUsage(func() {
		h.usageSvc.Log(context.Background(), &service.UsageEntry{
			RouteType:      "anthropic",
			ModelRequested: model,
			ProviderID:     providerID,
			APIKeyID:       keyID,
			TeamID:         teamID,
			Currency:       currency,
			StatusCode:     http.StatusBadGateway,
			ErrorType:      "provider_error",
			LatencyMs:      time.Since(start).Milliseconds(),
			FallbackCount:  fallbackCount,
			RetryCount:     retryCount,
		})
	})
}

func extractLastUserMessage(messages []domain.AnthropicMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return translator.ExtractContentText(messages[i].Content)
		}
	}
	return ""
}

func (h *AnthropicHandler) HandleMessages(c *gin.Context) {
	var req domain.AnthropicRequest
	var bodyBytes []byte
	if cached := middleware.GetBodyBytes(c); cached != nil {
		bodyBytes = cached
	} else {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, 10<<20))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "request too large"}})
			c.Abort()
			return
		}
		c.Request.Body.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"type":  "error",
			"error": gin.H{"type": "invalid_request_error", "message": safeProviderError(err)},
		})
		return
	}

	start := time.Now()
	sessionID := c.GetHeader("X-Session-ID")

	c.Set("model", req.Model)
	c.Set("stream", req.Stream)

	// Idempotency cache check (non-stream only)
	if !req.Stream {
		if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
			var idemKeyID int64
			if key := middleware.GetAPIKeyFromContext(c); key != nil {
				idemKeyID = key.ID
			}
			if cached, ok := h.idemCache.Get(c.Request.Context(), idemKeyID, idemKey); ok {
				safeHeaders := map[string]bool{"Content-Type": true, "X-Request-Id": true, "X-RateLimit-Limit": true, "X-RateLimit-Remaining": true, "X-RateLimit-Reset": true}
				for k, v := range cached.Headers {
					if safeHeaders[k] {
						c.Header(k, v)
					}
				}
				c.Data(cached.StatusCode, "application/json", cached.Body)
				return
			}
		}
	}

	if req.Stream {
		h.handleStream(c, &req, start, sessionID)
		return
	}

	result, err := h.svc.Chat(c.Request.Context(), &req, sessionID)
	if err != nil {
		h.logFailure(c, req.Model, start, err)
		h.writeError(c, err, req.Model)
		return
	}

	respBody, err := json.Marshal(result.Response)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": "marshal failed"}})
		return
	}

			// Extract identity for guardrail and usage logging
	var apiKeyID, teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}

	// Response-side guardrail check
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		var respTexts []string
		for _, block := range result.Response.Content {
			if block.Type == "text" {
				respTexts = append(respTexts, block.Text)
			}
		}
		respText := strings.Join(respTexts, "\n")
		if respText != "" {
			grResult, grErr := h.guardrailSvc.Check(c.Request.Context(), &guardrail.CheckRequest{
				Content:   respText,
				Direction: guardrail.DirectionResponse,
				Model:     req.Model,
				APIKeyID:  apiKeyID,
				TeamID:    teamID,
			})
				if grErr != nil {
					if h.guardrailSvc.IsFailOpen() {
						slog.Warn("guardrail: response check failed, fail-open", "error", grErr)
					} else {
						slog.Error("guardrail: response check failed", "error", grErr)
						c.JSON(http.StatusServiceUnavailable, gin.H{"type": "error", "error": gin.H{"type": "guardrail_error", "message": "guardrail service unavailable"}})
						return
					}
				} else if grResult != nil && grResult.Blocked {
					if grResult.Action == "block" {
						c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
						return
					}
					if grResult.Action == "mask" && grResult.MaskedContent != "" {
						c.Set("guardrail_triggered", true)
						c.Set("guardrail_rule", grResult.RuleName)
						result.Response.Content = []domain.ContentBlock{{Type: "text", Text: grResult.MaskedContent}}
						respBody, _ = json.Marshal(result.Response)
					}
					if grResult.Action == "log" {
						c.Set("guardrail_triggered", true)
						c.Set("guardrail_rule", grResult.RuleName)
					}
				} else if grResult != nil && grResult.Action == "log" {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
				}
			}
		}
	c.Header("Content-Type", "application/json")
	c.Status(http.StatusOK)
	c.Writer.Write(respBody)

	// Cache response for idempotency
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		h.idemCache.Set(c.Request.Context(), apiKeyID, idemKey, &service.CachedResponse{
				StatusCode: http.StatusOK,
				Body:       respBody,
			})
	}

	c.Set("provider", result.ProviderName)
	c.Set("input_tokens", result.InputTokens)
	c.Set("output_tokens", result.OutputTokens)
	c.Set("input_price", result.InputPrice)
	c.Set("output_price", result.OutputPrice)
	c.Set("usage_logged", true)

	submitUsage(func() {
		entry := &service.UsageEntry{
			RouteType:      "anthropic",
			ModelRequested: req.Model,
			ModelUsed:      result.ModelUsed,
			ProviderID:     result.ProviderID,
			APIKeyID:       apiKeyID,
			TeamID:         teamID,
			InputTokens:    result.InputTokens,
			OutputTokens:   result.OutputTokens,
			InputPrice:     result.InputPrice,
			OutputPrice:    result.OutputPrice,
			Currency:       result.Currency,
			LatencyMs:      result.LatencyMs,
			StatusCode:     http.StatusOK,
			FallbackCount:  result.FallbackCount,
			RetryCount:     result.RetryCount,
		}
		if v, ok := c.Get("guardrail_triggered"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTriggered = true
			}
		}
		if v, ok := c.Get("guardrail_rule"); ok {
			if s, _ := v.(string); s != "" {
				entry.GuardrailRule = s
			}
		}
		if v, ok := c.Get("agent_type"); ok {
			if s, _ := v.(string); s != "" {
				entry.AgentType = s
			}
		}
		if v, ok := c.Get("security_events"); ok {
			if d, _ := v.(datatypes.JSON); d != nil {
				entry.SecurityEvents = d
			}
		}
		if v, ok := c.Get("cache_hit"); ok {
			if b, _ := v.(bool); b {
				entry.CacheHit = true
			}
		}
		if h.usageSvc.IsContentLogEnabled() {
			entry.UserMessage = truncateContent(extractLastUserMessage(req.Messages))
			var texts []string
			for _, block := range result.Response.Content {
				if block.Type == "text" {
					texts = append(texts, block.Text)
				}
			}
			if len(texts) > 0 {
				entry.ModelResponse = truncateContent(strings.Join(texts, "\n"))
			}
		}
		h.usageSvc.Log(context.Background(), entry)
	})
}

func (h *AnthropicHandler) handleStream(c *gin.Context, req *domain.AnthropicRequest, start time.Time, sessionID string) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		slog.Error("response writer does not support flushing")
		c.JSON(http.StatusInternalServerError, gin.H{
			"type":  "error",
			"error": gin.H{"type": "api_error", "message": "streaming not supported"},
		})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	var modelRespBuf strings.Builder
	var firstTokenAt time.Time
	var apiKeyID, teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}

	var grWrapper *guardrail.CallbackStreamGuardrail
	var messageStopSent bool
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		grWrapper = guardrail.NewCallbackStreamGuardrail(h.guardrailSvc, req.Model, apiKeyID, teamID)
	}

	result, err := h.svc.StreamChat(c.Request.Context(), req, func(ctx context.Context, event service.StreamEvent) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		// Extract text from content_block_delta events (parsed once, used for TTFT + guardrail + content log)
		var deltaText string
		if event.Event == "content_block_delta" {
			var delta struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(event.Data), &delta) == nil {
				deltaText = delta.Delta.Text
			}
			if deltaText != "" && firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
		}

		// Response-side stream guardrail check
		if grWrapper != nil && deltaText != "" {
			grWrapper.Append(deltaText)
			if grWrapper.BufferLen() >= grWrapper.WindowSize() {
				blocked, grResult := grWrapper.CheckText(ctx, grWrapper.BufferText())
				grWrapper.Slide()
				if blocked {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
					if grResult.Action == "mask" && grResult.MaskedContent != "" {
						maskData, _ := json.Marshal(map[string]any{
							"type": "content_block_delta",
							"delta": map[string]any{"type": "text_delta", "text": grResult.MaskedContent},
						})
						fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", maskData)
						flusher.Flush()
						return true
					}
					reason := "blocked by guardrail"
					errData, _ := json.Marshal(map[string]any{
						"type":  "error",
						"error": map[string]any{"type": "guardrail_blocked", "message": reason},
					})
					fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
					fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					flusher.Flush()
					return false
				}
			}
		}

		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Event, event.Data)
		flusher.Flush()

		if event.Event == "message_stop" {
			messageStopSent = true
		}

		if h.usageSvc.IsContentLogEnabled() && deltaText != "" && modelRespBuf.Len() < maxResponseBuffer {
			modelRespBuf.WriteString(deltaText)
		}
		return true
	}, sessionID)

	// Final-drain: check any remaining buffered content after stream ends
	if grWrapper != nil && grWrapper.BufferLen() > 0 {
		blocked, grResult := grWrapper.FinalCheck(c.Request.Context(), grWrapper.BufferText())
		if blocked && grResult != nil {
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", grResult.RuleName)
			if grResult.Action == "mask" && grResult.MaskedContent != "" {
				maskData, _ := json.Marshal(map[string]any{
					"type": "content_block_delta",
					"delta": map[string]any{"type": "text_delta", "text": grResult.MaskedContent},
				})
				fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", maskData)
				if !messageStopSent {
					fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
				flusher.Flush()
				return
			}
			reason := "blocked by guardrail"
			errData, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "guardrail_blocked", "message": reason},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
			if !messageStopSent {
				fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}
			flusher.Flush()
			return
		}
		}

	if err != nil {
		slog.Error("stream error", "error", err, "model", req.Model)
		h.logFailure(c, req.Model, start, err)
		errData, _ := json.Marshal(map[string]any{
			"type":    "api_error",
			"message": safeProviderError(err),
		})
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
		flusher.Flush()
		return
	}

	c.Set("provider", result.ProviderName)
	c.Set("input_tokens", result.InputTokens)
	c.Set("output_tokens", result.OutputTokens)
	c.Set("input_price", result.InputPrice)
	c.Set("output_price", result.OutputPrice)
	c.Set("usage_logged", true)

	submitUsage(func() {
		entry := &service.UsageEntry{
			RouteType:      "anthropic",
			ModelRequested: req.Model,
			ModelUsed:      result.ModelUsed,
			ProviderID:     result.ProviderID,
			APIKeyID:       apiKeyID,
			TeamID:         teamID,
			InputTokens:    result.InputTokens,
			OutputTokens:   result.OutputTokens,
			InputPrice:     result.InputPrice,
			OutputPrice:    result.OutputPrice,
			Currency:       result.Currency,
			LatencyMs:      result.LatencyMs,
			FirstTokenMs:   firstTokenMsValue(start, firstTokenAt),
			StatusCode:     http.StatusOK,
			FallbackCount:  result.FallbackCount,
			RetryCount:     result.RetryCount,
		}
		if v, ok := c.Get("guardrail_triggered"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTriggered = true
			}
		}
		if v, ok := c.Get("guardrail_rule"); ok {
			if s, _ := v.(string); s != "" {
				entry.GuardrailRule = s
			}
		}
		if v, ok := c.Get("agent_type"); ok {
			if s, _ := v.(string); s != "" {
				entry.AgentType = s
			}
		}
		if v, ok := c.Get("security_events"); ok {
			if d, _ := v.(datatypes.JSON); d != nil {
				entry.SecurityEvents = d
			}
		}
		if v, ok := c.Get("cache_hit"); ok {
			if b, _ := v.(bool); b {
				entry.CacheHit = true
			}
		}
		if h.usageSvc.IsContentLogEnabled() {
			entry.UserMessage = truncateContent(extractLastUserMessage(req.Messages))
			entry.ModelResponse = truncateContent(modelRespBuf.String())
		}
		h.usageSvc.Log(context.Background(), entry)
	})
}

func (h *AnthropicHandler) writeError(c *gin.Context, err error, model string) {
	status := http.StatusInternalServerError

	switch {
	case errors.Is(err, translator.ErrMissingModel),
		errors.Is(err, translator.ErrMissingMessages),
		errors.Is(err, translator.ErrMissingMaxTokens):
		status = http.StatusBadRequest
	}

	var providerErr *provider.ProviderError
	if errors.As(err, &providerErr) {
		switch providerErr.StatusCode {
		case http.StatusTooManyRequests:
			status = http.StatusTooManyRequests
		case http.StatusUnauthorized, http.StatusForbidden:
			status = http.StatusBadGateway
		case http.StatusBadRequest:
			status = http.StatusBadRequest
		}
	}

	slog.Error("gateway error", "error", err, "model", model)
	c.JSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": "api_error", "message": safeProviderError(err)},
	})
}
