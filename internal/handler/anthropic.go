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

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/middleware"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
	"github.com/crosslink/pkg/token"
	"github.com/gin-gonic/gin"
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
	budgetSvc    *service.BudgetService      // pre-request budget reservation (C5); nil = disabled
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

func (h *AnthropicHandler) logFailure(c *gin.Context, model string, statusCode int, start time.Time, gatewayErr error, sessionID string) {
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
	// Error observability L1/L1.5: sanitized upstream message, status/type/
	// code/param and the failure stage. L2: per-provider attempt timeline
	// when the error came from routing.
	det := errorInfo(gatewayErr)
	var attempts datatypes.JSON
	if routeErr != nil {
		attempts = attemptsJSON(routeErr.Attempts)
	}
	c.Set("usage_logged", true)
	submitUsage(func() {
		h.usageSvc.Log(context.Background(), &service.UsageEntry{
			RouteType:          "anthropic",
			ModelRequested:     model,
			ProviderID:         providerID,
			APIKeyID:           keyID,
			TeamID:             teamID,
			OrgID:              orgID,
			Currency:           currency,
			StatusCode:         statusCode,
			ErrorType:          "provider_error",
			ErrorMessage:       det.Message,
			UpstreamStatus:     det.UpstreamStatus,
			UpstreamErrorCode:  det.Code,
			UpstreamErrorType:  det.Type,
			UpstreamErrorParam: det.Param,
			ErrorStage:         det.Stage,
			Attempts:           attempts,
			LatencyMs:          time.Since(start).Milliseconds(),
			FallbackCount:      fallbackCount,
			RetryCount:         retryCount,
			TemplateID:         templateID,
			PriceMultiplier:    priceMult,
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
			logRequestStageError(h.usageSvc, c, "anthropic", "", http.StatusBadRequest, "invalid_request_error", "request", err.Error())
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "request too large"}})
			c.Abort()
			return
		}
		c.Request.Body.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		logRequestStageError(h.usageSvc, c, "anthropic", req.Model, http.StatusBadRequest, "invalid_request_error", "request", err.Error())
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
			logRequestStageError(h.usageSvc, c, "anthropic", req.Model, http.StatusBadRequest, "invalid_request_error", "request", "capability modality mismatch: "+m.Modality)
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
				// Re-check the replayed body against the CURRENT response
				// policy — a cached response must never bypass updated rules. A
				// policy rejection REPLIES with an error: an idempotent replay
				// must never re-execute the (possibly side-effecting) operation.
				if !idemReplayPassesGuardrail(c, h.guardrailSvc, cached.Body, req.Model) {
					c.Set("skip_call_count", true) // rejected before any upstream work
					c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
					return
				}
				c.Set("skip_call_count", true) // idempotent replay: no new upstream work
				safeHeaders := map[string]bool{"Content-Type": true, "X-Request-Id": true, "X-RateLimit-Limit": true, "X-RateLimit-Remaining": true, "X-RateLimit-Reset": true}
				for k, v := range cached.Headers {
					if safeHeaders[k] {
						c.Header(k, v)
					}
				}
				c.Data(cached.StatusCode, "application/json", cached.Body)
				return
			}
			// Concurrent-duplicate guard: claim the key so a second request with
			// the same key cannot also execute while this one is in flight.
			if !h.idemCache.TryAcquire(c.Request.Context(), idemKeyID, idemKey) {
				c.Set("skip_call_count", true) // rejected before any upstream work
				c.JSON(http.StatusConflict, gin.H{
					"type":  "error",
					"error": gin.H{"type": "invalid_request_error", "message": "idempotency key is currently in use by another request"},
				})
				c.Abort()
				return
			}
			defer h.idemCache.Release(context.Background(), idemKeyID, idemKey)
		}
	}

	if req.Stream {
		h.handleStream(c, &req, maxCtx, start, sessionID)
		return
	}

	result, err := h.svc.Chat(c.Request.Context(), &req, sessionID, orgID)
	if err != nil {
		h.logFailure(c, req.Model, gatewayErrorStatus(err), start, err, sessionID)
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
		// Join checkable fragments (text, thinking, tool_use input) with
		// per-block offsets so span masks can be applied in place.
		var joined strings.Builder
		type blockRange struct{ start, end int }
		ranges := make([]blockRange, len(result.Response.Content))
		for bi := range result.Response.Content {
			block := &result.Response.Content[bi]
			var frag string
			switch block.Type {
			case "text", "thinking":
				frag = block.Text
			case "tool_use":
				if len(block.Input) > 0 {
					// Canonical form: JSON escapes decoded for detection
					// (the tool decodes them when parsing input).
					frag = canonicalArgsJSON(string(block.Input))
				}
			}
			if frag == "" {
				continue
			}
			if joined.Len() > 0 {
				joined.WriteString("\n")
			}
			ranges[bi].start = joined.Len()
			joined.WriteString(frag)
			ranges[bi].end = joined.Len()
		}
		respText := joined.String()
		if respText != "" {
			grCtx, grCancel := responseCheckCtx(c, h.guardrailSvc)
			defer grCancel()
			grResult, grErr := h.guardrailSvc.Check(grCtx, &guardrail.CheckRequest{
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
			}
			// Partial coverage is observable on every non-stream path: the
			// checked output exceeded the inspection limit and only its
			// prefix was scanned (usage_logs / security events pick this up).
			if grResult != nil && grResult.Truncated {
				c.Set("guardrail_truncated", true)
			}
			if grErr == nil && grResult != nil && grResult.Blocked {
				if grResult.Action == "block" {
					c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
					return
				}
				if grResult.Action == "log" {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
				}
			} else if grResult != nil && grResult.Action == "mask" {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", grResult.RuleName)
				if len(grResult.MaskedSpans) > 0 {
					// In-place per-block masking preserves structure.
					for bi := range result.Response.Content {
						r := ranges[bi]
						if r.end <= r.start {
							continue
						}
						masked := maskJoinedRange(respText, grResult.MaskedSpans, r.start, r.end)
						if masked == nil {
							continue
						}
						block := &result.Response.Content[bi]
						switch block.Type {
						case "text", "thinking":
							block.Text = *masked
						case "tool_use":
							if !json.Valid([]byte(*masked)) {
								// Masked tool input is no longer valid JSON —
								// refuse to emit unmasked input.
								c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
								return
							}
							block.Input = json.RawMessage(*masked)
						}
					}
					respBody, _ = json.Marshal(result.Response)
				} else if grResult.MaskedContent != "" {
					// Legacy whole-string mask cannot be mapped back to blocks
					// without collapsing structure (and leaving tool_use input
					// unmasked). Refuse to emit: controlled rejection.
					slog.Warn("guardrail: response mask without spans cannot be applied field-safely, blocking", "rule", grResult.RuleName)
					c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"}})
					return
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

	// Cache-aware cost so the usage row and ReportBudgetUsage agree (the budget
	// middleware prefers the precomputed "cost" context value).
	cost := anthropicCacheAdjustedCost(result.InputTokens, result.OutputTokens,
		result.CacheReadTokens, result.CacheCreationTokens, result.InputPrice, result.OutputPrice)
	c.Set("cost", cost)

	submitUsage(func() {
		entry := &service.UsageEntry{
			RouteType:       "anthropic",
			ModelRequested:  req.Model,
			ModelUsed:       result.ModelUsed,
			ProviderID:      result.ProviderID,
			APIKeyID:        apiKeyID,
			TeamID:          teamID,
			OrgID:           orgID,
			InputTokens:     result.InputTokens,
			OutputTokens:    result.OutputTokens,
			InputPrice:      result.InputPrice,
			OutputPrice:     result.OutputPrice,
			Currency:        result.Currency,
			LatencyMs:       result.LatencyMs,
			StatusCode:      http.StatusOK,
			FallbackCount:   result.FallbackCount,
			RetryCount:      result.RetryCount,
			ReasoningTokens: result.ReasoningTokens,
			CacheReadTokens: result.CacheReadTokens,
			SessionID:       sessionID,
			TemplateID:      templateID,
			PriceMultiplier: priceMult,
			PrecomputedCost: cost,
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
		if v, ok := c.Get("guardrail_truncated"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTruncated = true
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
	// Streaming responses are not replay-cached; the idempotency key is only
	// used to prevent concurrent duplicate execution.
	if idemKey := c.GetHeader("X-Idempotency-Key"); idemKey != "" && h.idemCache != nil {
		var idemKeyID int64
		if key := middleware.GetAPIKeyFromContext(c); key != nil {
			idemKeyID = key.ID
		}
		if !h.idemCache.TryAcquire(c.Request.Context(), idemKeyID, idemKey) {
			c.Set("skip_call_count", true) // rejected before any upstream work
			c.JSON(http.StatusConflict, gin.H{
				"type":  "error",
				"error": gin.H{"type": "invalid_request_error", "message": "idempotency key is currently in use by another request"},
			})
			c.Abort()
			return
		}
		defer h.idemCache.Release(context.Background(), idemKeyID, idemKey)
	}

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
	strictMode := grWrapper != nil && grWrapper.Strict()
	var strictOverflow bool

	result, err := h.svc.StreamChatWithConnect(c.Request.Context(), req, func(ctx context.Context, event service.StreamEvent) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}

		// Extract checkable fragments from content_block_delta AND
		// content_block_start events (parsed once, used for TTFT + guardrail +
		// content log). Block-start kinds: 3=text, 5=tool_use input, 4=thinking;
		// delta kinds: 0=text, 1=partial_json, 2=thinking.
		var deltaText, partialJSON, thinkingText string
		var startText, startInput, startThinking string
		blockIdx := -1
		switch event.Event {
		case "content_block_delta":
			var delta struct {
				Index int             `json:"index"`
				Delta json.RawMessage `json:"delta"`
			}
			if json.Unmarshal([]byte(event.Data), &delta) == nil {
				blockIdx = delta.Index
				var d struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
					Thinking    string `json:"thinking"`
				}
				if json.Unmarshal(delta.Delta, &d) == nil {
					switch d.Type {
					case "text_delta":
						deltaText = d.Text
					case "input_json_delta":
						partialJSON = d.PartialJSON
					case "thinking_delta":
						thinkingText = d.Thinking
					}
				}
			}
		case "content_block_start":
			var start struct {
				Index int `json:"index"`
				Block struct {
					Type     string          `json:"type"`
					Text     string          `json:"text"`
					Thinking string          `json:"thinking"`
					Input    json.RawMessage `json:"input"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(event.Data), &start) == nil && start.Block.Type != "" {
				blockIdx = start.Index
				switch start.Block.Type {
				case "text":
					startText = start.Block.Text
				case "tool_use":
					if len(start.Block.Input) > 0 {
						startInput = string(start.Block.Input)
					}
				case "thinking":
					startThinking = start.Block.Thinking
				}
			}
		}
		if (deltaText != "" || partialJSON != "" || thinkingText != "" || startText != "" || startInput != "" || startThinking != "") && firstTokenAt.IsZero() {
			firstTokenAt = time.Now()
		}

		if grWrapper != nil && strictMode {
			// Strict mode: buffer the raw event; nothing reaches the client
			// before StrictFinish clears it (check-before-emit). Cap overflow
			// fails closed by stopping the pull.
			if !grWrapper.BufferEvent(event.Event, event.Data) {
				strictOverflow = true
				return false
			}
			if blockIdx >= 0 {
				key := fmt.Sprintf("b%d", blockIdx)
				if (deltaText != "" && grWrapper.RecordDelta(key, 0, deltaText)) ||
					(partialJSON != "" && grWrapper.RecordDelta(key, 1, partialJSON)) ||
					(thinkingText != "" && grWrapper.RecordDelta(key, 2, thinkingText)) ||
					(startText != "" && grWrapper.RecordDelta(key, 3, startText)) ||
					(startInput != "" && grWrapper.RecordDelta(key, 5, startInput)) ||
					(startThinking != "" && grWrapper.RecordDelta(key, 4, startThinking)) {
					strictOverflow = true
					return false
				}
			}
			if event.Event == "message_stop" {
				messageStopSent = true
			}
			if h.usageSvc.IsContentLogEnabled() && deltaText != "" && modelRespBuf.Len() < maxResponseBuffer {
				modelRespBuf.WriteString(deltaText)
			}
			return true
		}

		// Response-side stream guardrail check (window mode): accumulate
		// text, tool-input JSON and thinking fragments (deltas AND block-start
		// initial content), check each window.
		if grWrapper != nil {
			if deltaText != "" {
				grWrapper.Append(deltaText)
			}
			if partialJSON != "" {
				grWrapper.Append(partialJSON)
			}
			if thinkingText != "" {
				grWrapper.Append(thinkingText)
			}
			if startText != "" {
				grWrapper.Append(startText)
			}
			if startInput != "" {
				grWrapper.Append(startInput)
			}
			if startThinking != "" {
				grWrapper.Append(startThinking)
			}
			if grWrapper.BufferLen() >= grWrapper.WindowSize() {
				blocked, grResult := grWrapper.CheckText(ctx, grWrapper.BufferText())
				grWrapper.Slide()
				if blocked {
					c.Set("guardrail_triggered", true)
					c.Set("guardrail_rule", grResult.RuleName)
					if grResult.Action == "mask" && grResult.MaskedContent != "" {
						maskData, _ := json.Marshal(map[string]any{
							"type":  "content_block_delta",
							"delta": map[string]any{"type": "text_delta", "text": grResult.MaskedContent},
						})
						if _, werr := fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", maskData); werr != nil {
							slog.Debug("anthropic stream: write failed, stopping", "error", werr)
							return false
						}
						flusher.Flush()
						return true
					}
					reason := "blocked by guardrail"
					errData, _ := json.Marshal(map[string]any{
						"type":  "error",
						"error": map[string]any{"type": "guardrail_blocked", "message": reason},
					})
					_, werr := fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
					if _, werr2 := fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"); werr2 != nil {
						werr = werr2
					}
					if werr != nil {
						slog.Debug("anthropic stream: write failed, stopping", "error", werr)
					}
					messageStopSent = true
					flusher.Flush()
					return false
				}
			}
		}

		if _, werr := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Event, event.Data); werr != nil {
			// Client is gone (or the write failed) â stop pulling from upstream.
			slog.Debug("anthropic stream: write failed, stopping", "error", werr)
			return false
		}
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

	// Response-side guardrail verdict.
	if strictOverflow {
		// Strict buffer cap exceeded â fail closed; buffered content dropped.
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", "guardrail_stream_cap")
		errData, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "guardrail_blocked", "message": "streamed response exceeds strict-mode guardrail buffer limit"},
		})
		fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
		if !messageStopSent {
			fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			messageStopSent = true
		}
		flusher.Flush()
	} else if grWrapper != nil && strictMode && err == nil {
		// Strict: the one and only release decision. Nothing was written
		// before this point.
		decision, events, grRes := grWrapper.StrictFinish(c.Request.Context())
		if decision == guardrail.StrictBlock {
			if grRes != nil {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", grRes.RuleName)
			}
			errData, _ := json.Marshal(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "guardrail_blocked", "message": "blocked by guardrail"},
			})
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", errData)
			if !messageStopSent {
				fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				messageStopSent = true
			}
			flusher.Flush()
		} else {
			if decision == guardrail.StrictMasked && grRes != nil {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", grRes.RuleName)
			}
			for _, ev := range events {
				if _, werr := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", ev.Name(), ev.Data()); werr != nil {
					slog.Debug("anthropic stream: write failed during strict release", "error", werr)
					break
				}
			}
			flusher.Flush()
		}
	} else if grWrapper != nil && !strictMode && grWrapper.BufferLen() > 0 {
		// Window mode final-drain: check any remaining buffered content.
		blocked, grResult := grWrapper.FinalCheck(c.Request.Context(), grWrapper.BufferText())
		if blocked && grResult != nil {
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", grResult.RuleName)
			if grResult.Action == "mask" && grResult.MaskedContent != "" {
				maskData, _ := json.Marshal(map[string]any{
					"type":  "content_block_delta",
					"delta": map[string]any{"type": "text_delta", "text": grResult.MaskedContent},
				})
				fmt.Fprintf(c.Writer, "event: content_block_delta\ndata: %s\n\n", maskData)
				if !messageStopSent {
					fmt.Fprintf(c.Writer, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
				flusher.Flush()
				// Upstream tokens were already consumed â fall through to
				// submitUsage so the request is recorded.
				messageStopSent = true
			} else {
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
				// Upstream tokens were already consumed â fall through to
				// submitUsage so the request is recorded.
				messageStopSent = true
			}
		}
	}

	if err != nil {
		slog.Error("stream error", "error", err, "model", req.Model)
		h.logFailure(c, req.Model, gatewayErrorStatus(err), start, err, sessionID)
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

	// Cache-aware cost (see the non-stream path): keeps the usage row and
	// ReportBudgetUsage on the same number.
	cost := anthropicCacheAdjustedCost(result.InputTokens, result.OutputTokens,
		result.CacheReadTokens, result.CacheCreationTokens, result.InputPrice, result.OutputPrice)
	c.Set("cost", cost)

	submitUsage(func() {
		entry := &service.UsageEntry{
			RouteType:       "anthropic",
			ModelRequested:  req.Model,
			ModelUsed:       result.ModelUsed,
			ProviderID:      result.ProviderID,
			APIKeyID:        apiKeyID,
			TeamID:          teamID,
			OrgID:           orgID,
			InputTokens:     result.InputTokens,
			OutputTokens:    result.OutputTokens,
			InputPrice:      result.InputPrice,
			OutputPrice:     result.OutputPrice,
			Currency:        result.Currency,
			LatencyMs:       result.LatencyMs,
			FirstTokenMs:    firstTokenMsValue(start, firstTokenAt),
			StatusCode:      http.StatusOK,
			FallbackCount:   result.FallbackCount,
			RetryCount:      result.RetryCount,
			ReasoningTokens: result.ReasoningTokens,
			CacheReadTokens: result.CacheReadTokens,
			SessionID:       sessionID,
			TemplateID:      templateID,
			PriceMultiplier: priceMult,
			PrecomputedCost: cost,
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
		if v, ok := c.Get("guardrail_truncated"); ok {
			if b, _ := v.(bool); b {
				entry.GuardrailTruncated = true
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
		// Stream: UsageFromUpstr marks real upstream usage â the translator seeds
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

// anthropicCacheAdjustedCost applies Anthropic prompt-cache pricing on top of
// the plain token cost. Cache-read tokens are included in inputTokens (OpenAI
// semantics) but Anthropic bills them at 0.1Ã the input price, so discount the
// 0.9Ã; cache-creation (write) tokens are billed at 1.25Ã, so add the 0.25Ã.
func anthropicCacheAdjustedCost(inputTokens, outputTokens, cacheRead, cacheCreation int, inputPrice, outputPrice float64) float64 {
	cost := inputPrice*float64(inputTokens)/1000 + outputPrice*float64(outputTokens)/1000
	if cacheRead > 0 {
		cost -= inputPrice * float64(cacheRead) * 0.9 / 1000
	}
	if cacheCreation > 0 {
		cost += inputPrice * float64(cacheCreation) * 0.25 / 1000
	}
	if cost < 0 {
		cost = 0
	}
	return cost
}

// gatewayErrorStatus maps a GatewayService error to the client-facing HTTP
// status. Shared by writeError and logFailure so the client response and the
// usage log always record the same code (the log previously hardcoded 502,
// masking upstream 400s as server errors).
func gatewayErrorStatus(err error) int {
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

	return status
}

func (h *AnthropicHandler) writeError(c *gin.Context, err error, model string) {
	slog.Error("gateway error", "error", err, "model", model)
	// Upstream 429 must surface as rate_limit_error + Retry-After so clients
	// can back off with correct semantics instead of treating it as a generic
	// API failure.
	errType := "api_error"
	if providerRateLimited(err) {
		errType = "rate_limit_error"
		providerRetryAfterHeader(c, err)
	}
	c.JSON(gatewayErrorStatus(err), gin.H{
		"type":  "error",
		"error": gin.H{"type": errType, "message": safeProviderError(err)},
	})
}

// maskJoinedRange applies non-overlapping guardrail spans to the [start,end)
// fragment of a joined content string and returns the masked fragment, or nil
// when no span touches the range.
func maskJoinedRange(content string, spans []guardrail.Span, start, end int) *string {
	var b strings.Builder
	touched := false
	last := start
	for _, sp := range spans {
		if sp.End <= start || sp.Start >= end {
			continue
		}
		touched = true
		s := sp.Start
		if s < start {
			s = start
		}
		e := sp.End
		if e > end {
			e = end
		}
		if s > last {
			b.WriteString(content[last:s])
		}
		b.WriteString(sp.Replacement)
		last = e
	}
	if !touched {
		return nil
	}
	if last < end {
		b.WriteString(content[last:end])
	}
	out := b.String()
	return &out
}

// idemReplayPassesGuardrail re-checks an idempotency-cached body against the
// current response-direction policy before replaying it. True when guardrails
// are off or the body passes; false when the body is blocked, needs masking,
// or the check failed fail-closed — the caller must REJECT the replay with an
// error, never fall through: an idempotent replay must not re-execute the
// original (possibly side-effecting) operation under a changed policy.
func idemReplayPassesGuardrail(c *gin.Context, svc *guardrail.GuardrailService, body []byte, model string) bool {
	if svc == nil || !svc.IsEnabled() {
		return true
	}
	text := extractGuardrailTextFromBody(body, c.Request.URL.Path)
	if text == "" {
		return true
	}
	var apiKeyID, teamID int64
	if key := middleware.GetAPIKeyFromContext(c); key != nil {
		apiKeyID = key.ID
		if key.TeamID != nil {
			teamID = *key.TeamID
		}
	}
	orgID := c.GetInt64("org_id")

	guardCtx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(svc.ResponseBudgetMs())*time.Millisecond)
	defer cancel()
	result, err := svc.Check(guardCtx, &guardrail.CheckRequest{
		Content:   text,
		Direction: guardrail.DirectionResponse,
		Model:     model,
		APIKeyID:  apiKeyID,
		TeamID:    teamID,
		OrgID:     orgID,
	})
	if err != nil {
		if svc.IsFailOpen() {
			return true
		}
		return false
	}
	if result == nil {
		return true
	}
	if result.Action == "log" && result.RuleName != "" {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", result.RuleName)
	}
	if result.Truncated {
		c.Set("guardrail_truncated", true)
	}
	if result.Blocked || result.Action == "mask" {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", result.RuleName)
		return false
	}
	return true
}

// extractGuardrailTextFromBody pulls the checkable text of a serialized
// chat/completions or messages response body.
func extractGuardrailTextFromBody(body []byte, path string) string {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	var parts []string
	if strings.HasSuffix(path, "/v1/chat/completions") {
		var choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				ToolCalls        []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		}
		if json.Unmarshal(root["choices"], &choices) != nil {
			return ""
		}
		for _, ch := range choices {
			var s string
			if json.Unmarshal(ch.Message.Content, &s) == nil && s != "" {
				parts = append(parts, s)
			} else {
				var contentParts []map[string]json.RawMessage
				if json.Unmarshal(ch.Message.Content, &contentParts) == nil {
					for _, p := range contentParts {
						var t string
						if json.Unmarshal(p["text"], &t) == nil && t != "" {
							parts = append(parts, t)
						}
					}
				}
			}
			if ch.Message.ReasoningContent != "" {
				parts = append(parts, ch.Message.ReasoningContent)
			}
			for _, tc := range ch.Message.ToolCalls {
				if tc.Function.Arguments != "" {
					parts = append(parts, canonicalArgsJSON(tc.Function.Arguments))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if strings.HasSuffix(path, "/v1/messages") {
		var resp struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal(root["content"], &resp.Content) != nil {
			return ""
		}
		for _, b := range resp.Content {
			switch b.Type {
			case "text", "thinking":
				if b.Text != "" {
					parts = append(parts, b.Text)
				}
			case "tool_use":
				if len(b.Input) > 0 {
					parts = append(parts, canonicalArgsJSON(string(b.Input)))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// responseCheckCtx returns the context for a response-direction guardrail
// check with the configured response budget applied.
func responseCheckCtx(c *gin.Context, svc *guardrail.GuardrailService) (context.Context, context.CancelFunc) {
	ctx := c.Request.Context()
	if svc != nil {
		if budget := svc.ResponseBudgetMs(); budget > 0 {
			return context.WithTimeout(ctx, time.Duration(budget)*time.Millisecond)
		}
	}
	return ctx, func() {}
}

// canonicalArgsJSON decodes and re-encodes a JSON arguments/input string so
// escaped sequences (\uXXXX) are checked in DECODED form: a tool receiving
// {"value":"SE\u0043RET"} sees "SECRET", so detection must too. Masked
// rewrites are written back in this canonical form (validated as JSON).
func canonicalArgsJSON(raw string) string {
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return raw
}
