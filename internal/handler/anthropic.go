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
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
	"github.com/crosslink/pkg/token"
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
	svc          *service.GatewayService
	resolver     *router.Resolver
	usageSvc     *service.UsageService
	idemCache    *service.IdempotencyCache
	guardrailSvc *guardrail.GuardrailService
	budgetSvc    *service.BudgetService // pre-request budget reservation (C5); nil = disabled
	calibration  *service.CalibrationService // context analysis calibration; nil = disabled (Task 9 wires it)
}

func NewAnthropicHandler(svc *service.GatewayService, resolver *router.Resolver, usageSvc *service.UsageService, idemCache *service.IdempotencyCache, guardrailSvc *guardrail.GuardrailService) *AnthropicHandler {
	return &AnthropicHandler{svc: svc, resolver: resolver, usageSvc: usageSvc, idemCache: idemCache, guardrailSvc: guardrailSvc}
}

// SetBudgetSvc injects the budget service used for pre-request budget
// reservation (concurrency-safe enforcement, C5). Optional; nil disables it.
func (h *AnthropicHandler) SetBudgetSvc(b *service.BudgetService) { h.budgetSvc = b }

// SetCalibration injects the token-estimation calibration service
// (context analysis). Optional; nil disables calibration.
func (h *AnthropicHandler) SetCalibration(c *service.CalibrationService) { h.calibration = c }

// estimateAnthropicInputTokens rough-estimates the input token count of an
// Anthropic /v1/messages request, for pre-request budget reservation (C5).
func estimateAnthropicInputTokens(req *domain.AnthropicRequest) int {
	if req == nil {
		return 0
	}
	n := 0
	for _, msg := range req.Messages {
		n += token.Estimate(translator.ExtractContentText(msg.Content))
	}
	if len(req.System) > 0 {
		n += token.Estimate(string(req.System))
	}
	return n
}

func (h *AnthropicHandler) logFailure(c *gin.Context, model string, start time.Time, gatewayErr error, sessionID string) {
	var keyID int64
	var teamID int64
	orgID := c.GetInt64("org_id")
	templateID := readTemplateID(c)
	priceMult := readPriceMultiplier(c)
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
			OrgID:          orgID,
			Currency:       currency,
			StatusCode:     http.StatusBadGateway,
			ErrorType:      "provider_error",
			LatencyMs:      time.Since(start).Milliseconds(),
			FallbackCount:  fallbackCount,
			RetryCount:     retryCount,
			TemplateID:     templateID,
				PriceMultiplier: priceMult,
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

	// Modality guard: a capability alias must match this endpoint's modality.
	orgID := c.GetInt64("org_id")
	templateID := readTemplateID(c)
	priceMult := readPriceMultiplier(c)
	if m, ok := h.resolver.AliasMetaLookup(c.Request.Context(), req.Model, orgID); ok {
		if m.Modality != string(domain.ModalityText) {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "capability modality mismatch"}})
			return
		}
		c.Header("x-crosslink-capability", m.Name)
	}

	// C5: concurrency-safe budget reservation against the primary route's price,
	// closing the check-then-act race in BudgetCheck's GET-based check. The
	// Anthropic path delegates routing to GatewayService, so resolve the primary
	// route here solely for the price estimate (resolver is cache-backed, cheap).
	var maxCtx *int // primary route's context window, captured synchronously (analysis input)
	if routes, rerr := h.resolver.Resolve(c.Request.Context(), req.Model, orgID); rerr == nil && len(routes) > 0 {
		maxCtx = routes[0].MaxContext
		if !reserveBudgetForRequest(c, h.budgetSvc, estimateAnthropicInputTokens(&req), req.MaxTokens, routes[0].InputPrice, routes[0].OutputPrice) {
			return
		}
	}

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
		h.handleStream(c, &req, maxCtx, start, sessionID)
		return
	}

	result, err := h.svc.Chat(c.Request.Context(), &req, sessionID, orgID)
	if err != nil {
		h.logFailure(c, req.Model, start, err, sessionID)
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
				OrgID:     orgID,
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
	setFallbackHeaders(c, result.ModelUsed, result.FallbackCount)
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
			OrgID:          orgID,
			InputTokens:    result.InputTokens,
			OutputTokens:   result.OutputTokens,
			InputPrice:     result.InputPrice,
			OutputPrice:    result.OutputPrice,
			Currency:       result.Currency,
			LatencyMs:      result.LatencyMs,
			StatusCode:      http.StatusOK,
			FallbackCount:   result.FallbackCount,
			RetryCount:      result.RetryCount,
			ReasoningTokens: result.ReasoningTokens,
			CacheReadTokens: result.CacheReadTokens,
			SessionID:       sessionID,
			TemplateID:      templateID,
				PriceMultiplier: priceMult,
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
		BuildContextAnalysisResult(contextAnalysisInput{
			anthropicReq:   &req,
			maxContext:     maxCtx,
			maxTokens:      req.MaxTokens,
			modelUsed:      result.ModelUsed,
			observeFn:      calibrationObserveOf(h.calibration),
			inputFromUpstr: result.InputTokens > 0,
			inputTokens:    result.InputTokens,
		}).apply(entry)
		h.usageSvc.Log(context.Background(), entry)
	})
}

func (h *AnthropicHandler) handleStream(c *gin.Context, req *domain.AnthropicRequest, maxCtx *int, start time.Time, sessionID string) {
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
	orgID := c.GetInt64("org_id")
	templateID := readTemplateID(c)
	priceMult := readPriceMultiplier(c)

	var grWrapper *guardrail.CallbackStreamGuardrail
	var messageStopSent bool
	if h.guardrailSvc != nil && h.guardrailSvc.IsEnabled() {
		grWrapper = guardrail.NewCallbackStreamGuardrail(h.guardrailSvc, req.Model, apiKeyID, teamID, orgID)
	}

	result, err := h.svc.StreamChatWithConnect(c.Request.Context(), req, func(ctx context.Context, event service.StreamEvent) bool {
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
					messageStopSent = true
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
	}, func(route *router.RouteResult, fallbackCount int) {
		// Connection (and any fallback) has settled but no event has been written yet,
		// so route-dependent headers can still take effect before the streamed body.
		setFallbackHeaders(c, route.ProviderModel, fallbackCount)
	}, sessionID, orgID)

	// C6-closure: publish token usage on every exit so ReportTokens can reconcile
	// the TPM reservation made by TPMLimit. Early returns below (guardrail final
	// block, stream error) used to skip c.Set("input_tokens"/"output_tokens"),
	// leaking the reservation. The normal path sets them inline first; this defer
	// only backfills the early-return cases.
	defer func() {
		if _, ok := c.Get("input_tokens"); ok {
			return
		}
		it, ot := result.InputTokens, result.OutputTokens
		if it == 0 {
			it = estimateAnthropicInputTokens(req)
		}
		c.Set("input_tokens", it)
		c.Set("output_tokens", ot)
		c.Set("input_price", result.InputPrice)
		c.Set("output_price", result.OutputPrice)
	}()

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
		h.logFailure(c, req.Model, start, err, sessionID)
		errData, _ := json.Marshal(map[string]any{
			"type":    "api_error",
			"message": safeProviderError(err),
		})
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
		flusher.Flush()
		return
	}

	// Graceful degradation: if the upstream stream ended without a terminal message_stop
	// (mid-stream disconnect), emit a structured stream_interrupted event so clients can
	// tell a truncation from a clean finish, then close with message_stop.
	if !messageStopSent {
		slog.Warn("anthropic stream ended without message_stop, sending graceful end",
			"provider", result.ProviderName,
			"output_tokens", result.OutputTokens)
		writeStreamInterruptedAnthropic(c.Writer)
		flusher.Flush()
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
			OrgID:          orgID,
			InputTokens:    result.InputTokens,
			OutputTokens:   result.OutputTokens,
			InputPrice:     result.InputPrice,
			OutputPrice:    result.OutputPrice,
			Currency:       result.Currency,
			LatencyMs:      result.LatencyMs,
			FirstTokenMs:    firstTokenMsValue(start, firstTokenAt),
			StatusCode:      http.StatusOK,
			FallbackCount:   result.FallbackCount,
			RetryCount:      result.RetryCount,
			ReasoningTokens: result.ReasoningTokens,
			CacheReadTokens: result.CacheReadTokens,
			SessionID:       sessionID,
			TemplateID:      templateID,
				PriceMultiplier: priceMult,
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
		// Stream: UsageFromUpstr marks real upstream usage — the translator seeds
		// InputTokens with an estimate that must not feed calibration.
		BuildContextAnalysisResult(contextAnalysisInput{
			anthropicReq:   req,
			maxContext:     maxCtx,
			maxTokens:      req.MaxTokens,
			modelUsed:      result.ModelUsed,
			observeFn:      calibrationObserveOf(h.calibration),
			inputFromUpstr: result.UsageFromUpstr,
			inputTokens:    result.InputTokens,
		}).apply(entry)
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
	case errors.Is(err, router.ErrProRequired):
		status = http.StatusForbidden
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
	// Upstream 429 must surface as rate_limit_error + Retry-After so clients
	// can back off with correct semantics instead of treating it as a generic
	// API failure.
	errType := "api_error"
	if providerRateLimited(err) {
		errType = "rate_limit_error"
		providerRetryAfterHeader(c, err)
	}
	c.JSON(status, gin.H{
		"type":  "error",
		"error": gin.H{"type": errType, "message": safeProviderError(err)},
	})
}
