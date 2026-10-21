package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"context"
	"time"

	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

const maxCacheResponseSize = 10 << 20 // 10MB max cached response

type responseCapture struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *responseCapture) Write(data []byte) (int, error) {
	if w.body.Len() < maxCacheResponseSize {
		w.body.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *responseCapture) WriteString(s string) (int, error) {
	if w.body.Len() < maxCacheResponseSize {
		w.body.WriteString(s)
	}
	return w.ResponseWriter.WriteString(s)
}

// CacheHitBiller bills one semantic-cache HIT: it must produce exactly one
// durable usage row (cache_hit=true) through the same submit pipeline the
// handlers use. Wired in app.go to handler.NewCacheHitBiller.
type CacheHitBiller func(c *gin.Context, cached *service.CacheEntry)

// Cache returns a middleware that caches non-streaming responses in Redis via CacheService.
// It must be mounted INNERMOST (after BudgetCheck/TPM/ReportBudgetUsage, just
// before the handlers): a cache HIT still passes admission control and is
// billed and budget-settled like any served request (r63 probe defect: 40
// identical 200s, only the populating MISS was billed — HITs aborted the
// chain above BudgetCheck and served free of charge, invisible to
// usage_logs and to budget enforcement).
// Pass nil cacheSvc to disable caching (Community mode).
// guardrailSvc may be nil (guardrails disabled); when present, every cache HIT
// is re-checked against the CURRENT response-direction guardrail policy — a
// block (or a mask that would rewrite the body) bypasses the cache and falls
// through to the upstream pipeline, so a stale cached response can never leak
// content that today's policy forbids.
func Cache(cacheSvc service.CacheServiceInterface, cp crypto.CryptoProvider, guardrailSvc *guardrail.GuardrailService, billHit CacheHitBiller) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cacheSvc == nil {
			c.Next()
			return
		}
		hitStart := time.Now()

		if !cacheSvc.IsEnabled() {
			c.Next()
			return
		}

		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Skip non-cacheable endpoints before reading body
		path := c.Request.URL.Path
		if path == "/v1/images/generations" ||
			strings.HasPrefix(path, "/v1/videos") ||
			strings.HasPrefix(path, "/v1/audio") ||
			strings.HasPrefix(path, "/v1/batch") ||
			strings.HasPrefix(path, "/v1/responses") {
			c.Next()
			return
		}

		bodyBytes := GetBodyBytes(c)
		if bodyBytes == nil {
			var err error
			bodyBytes, err = io.ReadAll(io.LimitReader(c.Request.Body, int64(cacheSvc.MaxBodySize())))
			if err != nil {
				c.Next()
				return
			}
			c.Request.Body.Close()
			c.Request.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}

		var probe struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		if json.Unmarshal(bodyBytes, &probe) != nil {
			c.Next()
			return
		}
		if probe.Stream {
			c.Next()
			return
		}

		// An explicit X-Idempotency-Key hands replay semantics to the
		// handler's idempotency layer (bill exactly once per event). The
		// semantic cache must not intercept it: an identical body under the
		// same idem key is a REPLAY, not a new serving — a HIT here would
		// re-bill what the idem layer guarantees to bill once.
		if c.GetHeader("X-Idempotency-Key") != "" {
			c.Next()
			return
		}

		ctx := c.Request.Context()

		modelTTL, disabled := cacheSvc.GetModelCacheConfig(ctx, probe.Model)
		if disabled {
			c.Header("X-Cache", "BYPASS")
			c.Next()
			return
		}

		ttl := cacheSvc.GetTTLForEndpoint(c.Request.URL.Path)
		if modelTTL > 0 {
			ttl = modelTTL
		}

		key := buildCacheKey(c.Request.URL.Path, bodyBytes, c, cp)

		cached, ok := cacheSvc.Get(ctx, key)
		bypassUnpriced := ok && cached != nil && !cached.Priced
		if bypassUnpriced {
			// Legacy entry cached before price snapshots existed: a serving
			// from it cannot be billed (no prices to settle against) — a
			// declared zero-cost grace period is NOT acceptable (r65
			// review). Fall through to the upstream pipeline instead: the
			// request re-executes and re-bills at today's prices, and the
			// capture below overwrites the entry with a priced snapshot. A
			// genuine zero-price model is Priced=true and serves normally.
			c.Header("X-Cache", "BYPASS-UNPRICED")
			RecordCacheBypassUnpriced(probe.Model)
		}
		if ok && cached != nil && !bypassUnpriced {
			if !cacheHitPassesGuardrail(c, guardrailSvc, cached.Body, path, probe.Model) {
				// Today's response policy rejects (or wants to mask) the
				// cached body: fall through to the upstream pipeline so the
				// content is regenerated under the current rules. The stale
				// entry is never served.
				c.Header("X-Cache", "BYPASS-GUARDRAIL")
				c.Next()
				return
			}
			c.Header("X-Cache", "HIT")
			c.Header("Content-Type", "application/json")
			c.Status(http.StatusOK)
			c.Writer.Write(cached.Body)
			c.Set("model", probe.Model)
			c.Set("provider", "cache")
			c.Set("cache_hit", true)
			if cached.InputTokens > 0 || cached.OutputTokens > 0 {
				c.Set("input_tokens", cached.InputTokens)
				c.Set("output_tokens", cached.OutputTokens)
			}
			// Restore the billing snapshot so the unwinding settle middleware
			// (ReportBudgetUsage) prices the HIT exactly like the request that
			// populated the entry. Without prices the settle would compute
			// cost 0 and skip — the HIT would again be unbilled.
			if cached.InputPrice > 0 || cached.OutputPrice > 0 {
				c.Set("input_price", cached.InputPrice)
				c.Set("output_price", cached.OutputPrice)
			}
			if cached.Currency != "" {
				c.Set("currency", cached.Currency)
			}
			// One durable ledger row per served HIT, before the abort cuts
			// the chain down to the handlers (the row submit itself is
			// WAL-first and never blocks on this response path).
			if billHit != nil {
				c.Set("cache_hit_latency_ms", time.Since(hitStart).Milliseconds())
				billHit(c, cached)
			}
			RecordCacheHit(probe.Model)
			c.Abort()
			return
		}

		if !bypassUnpriced {
			c.Header("X-Cache", "MISS")
			RecordCacheMiss(probe.Model)
		}

		// NOTE: Thundering herd risk on cache miss — concurrent identical requests
		// will all hit the upstream provider. For this use case (LLM API proxy),
		// occasional thundering herd is acceptable because cache TTL is long (5–60 min)
		// and request volume per key is typically low. A singleflight approach was
		// considered but is complex to implement correctly with gin.Context's c.Next().

		capture := &responseCapture{ResponseWriter: c.Writer}
		c.Writer = capture
		c.Next()

		if capture.Status() == http.StatusOK && capture.body.Len() > 0 {
			// Snapshot the serving prices the handler just published on the
			// context, so future HITs of this entry re-bill at the same
			// prices (entries from price-less responses keep zeros: their
			// HITs settle cost 0 until TTL ages them out).
			inPrice, _ := c.Get("input_price")
			outPrice, _ := c.Get("output_price")
			currency, _ := c.Get("currency")
			ip, _ := inPrice.(float64)
			op, _ := outPrice.(float64)
			cur, _ := currency.(string)
			if err := cacheSvc.Set(ctx, key, probe.Model, c.Request.URL.Path, capture.body.Bytes(), ttl, ip, op, cur); err != nil {
				slog.Warn("cache set failed", "error", err)
			}
		}
	}
}

// buildCacheKey generates a deterministic cache key from the request path, body, and caller identity.
// Excludes non-semantic fields: stream, stream_options, user.
// Includes api_key_id or user_id to prevent cross-user cache poisoning.
func buildCacheKey(path string, body []byte, c *gin.Context, cp crypto.CryptoProvider) string {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return "cl:cache:" + cp.HashHex(body)
	}
	delete(raw, "stream")
	delete(raw, "stream_options")
	delete(raw, "user")
	delete(raw, "x_context") // defensive: ContextAssembler already strips it pre-cache

	canonical, _ := json.Marshal(raw)

	// Include caller identity to isolate cache between users
	var identity string
	if keyID, exists := c.Get("api_key_id"); exists {
		if id, ok := keyID.(int64); ok {
			identity = fmt.Sprintf("key:%d", id)
		}
	}
	if identity == "" {
		if userID, exists := c.Get("user_id"); exists {
			if id, ok := userID.(int64); ok {
				identity = fmt.Sprintf("user:%d", id)
			}
		}
	}

	input := path + "\n" + identity + "\n"
	return "cl:cache:" + cp.HashHex(append([]byte(input), canonical...))
}

// cacheHitPassesGuardrail re-checks a cached response body against the current
// response-direction policy. True when guardrails are off or the body passes
// (log-only); false when the body is blocked, needs masking, or the check
// failed under fail-closed — in all those cases the cache must not be served.
func cacheHitPassesGuardrail(c *gin.Context, svc *guardrail.GuardrailService, body []byte, path, model string) bool {
	if svc == nil || !svc.IsEnabled() {
		return true
	}
	text := extractCachedResponseText(body, path)
	if text == "" {
		return true
	}
	var apiKeyID, teamID int64
	if key := GetAPIKeyFromContext(c); key != nil {
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
			slog.Warn("guardrail: cache-hit recheck failed, fail-open (serving cache)", "error", err)
			return true
		}
		slog.Error("guardrail: cache-hit recheck failed, bypassing cache", "error", err)
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
		c.Header("X-Guardrail-Coverage", "partial")
		c.Set("guardrail_truncated", true)
	}
	// Mask results are also bypassed: correctly rewriting the cached body is
	// not possible here without duplicating handler masking, so the request
	// regenerates through the full pipeline.
	if result.Blocked || result.Action == "mask" {
		c.Set("guardrail_triggered", true)
		c.Set("guardrail_rule", result.RuleName)
		return false
	}
	return true
}

// extractCachedResponseText pulls the checkable text (message content,
// reasoning, tool-call arguments — all choices) out of a cached non-stream
// response body for chat/completions and messages formats.
func extractCachedResponseText(body []byte, path string) string {
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
			if s, ok := rawString(ch.Message.Content); ok && s != "" {
				parts = append(parts, s)
			} else {
				var contentParts []map[string]json.RawMessage
				if json.Unmarshal(ch.Message.Content, &contentParts) == nil {
					for _, p := range contentParts {
						if t, ok := rawString(p["text"]); ok && t != "" {
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
					parts = append(parts, tc.Function.Arguments)
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
		if json.Unmarshal(root["content"], &resp.Content) != nil && root["content"] == nil {
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
					parts = append(parts, string(b.Input))
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
