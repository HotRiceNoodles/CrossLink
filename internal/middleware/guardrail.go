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

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

const maxGuardrailBodySize = 10 << 20

func GuardrailsRequest(svc *guardrail.GuardrailService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if svc == nil || !svc.IsEnabled() {
			c.Next()
			return
		}

		// Multipart file-upload bodies (audio transcription/translation, image
		// edits) carry binary media the text guardrail cannot inspect — and
		// MUST NOT be buffered here: a truncated (maxGuardrailBodySize) copy
		// replacing the original body corrupts the multipart framing for
		// uploads larger than that (body.go exempts the same path set from
		// ReadBody for exactly this reason). Under strict coverage they are
		// rejected below; otherwise they pass through (documented gap).
		multipartFile := strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") &&
			(isFileUploadPath(c.Request.URL.Path) ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/transcribe") ||
				strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground/translate"))

		if c.Request.Method != http.MethodPost {
			c.Next()
			return
		}

		// Skip admin routes (except playground which proxies LLM calls)
		if strings.HasPrefix(c.Request.URL.Path, "/admin/api/") && !strings.HasPrefix(c.Request.URL.Path, "/admin/api/playground") {
			c.Next()
			return
		}

		if multipartFile && !svc.StrictCoverage() {
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
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(bodyBytes, &probe) != nil && !multipartFile {
			c.Next()
			return
		}

		ex := guardrail.ExtractContent(bodyBytes, c.Request.URL.Path)
		streamContent := ""
		if ex != nil {
			streamContent = ex.Text
		}
		piiCtx, piiVault, piiErr := guardrail.WithPIIVault(c.Request.Context())
		if piiErr != nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "PII mapping unavailable"}})
			return
		}
		c.Request = c.Request.WithContext(piiCtx)
		defer piiVault.Clear()
		next := func() {
			if c.IsAborted() {
				return
			}
			if piiVault.Active() {
				if c.GetHeader("X-Idempotency-Key") != "" {
					c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "reversible PII masking does not support X-Idempotency-Key; mappings are request-scoped"}})
					return
				}
				finish := installPIIRestore(c, piiVault, svc.StrictMaxBytes(), svc.StrictMaxEvents())
				defer finish()
			}
			// Response engines are opened only after request-side approval;
			// contextual engines receive the transformed request when masked.
			if probe.Stream && svc.StreamMode() == "incremental" {
				meta := guardrail.CheckRequest{Content: streamContent, Model: probe.Model, Direction: guardrail.DirectionResponse, OrgID: c.GetInt64("org_id")}
				if key := GetAPIKeyFromContext(c); key != nil {
					meta.APIKeyID = key.ID
					if key.TeamID != nil {
						meta.TeamID = *key.TeamID
					}
				}
				finish, err := installIncrementalStream(c, svc, meta)
				if err != nil {
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"type": "guardrail_error", "message": "stream guardrail unavailable"}})
					return
				}
				defer finish()
			}
			c.Next()
		}

		// Strict coverage: a POST gateway content endpoint whose body we cannot
		// structurally inspect — no extractable text, or non-text media parts
		// riding alongside it — must be rejected, never silently passed.
		if svc.StrictCoverage() && isGatewayContentPath(c.Request.URL.Path) &&
			(ex == nil || ex.Uninspectable) {
			reason := "endpoint content cannot be inspected by the guardrail under strict coverage policy"
			switch {
			case multipartFile:
				reason = "multipart media content cannot be inspected by the text guardrail under strict coverage policy"
			case ex != nil && ex.Uninspectable:
				reason = "request contains non-text media parts that cannot be inspected by the text guardrail under strict coverage policy"
			}
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"type": "guardrail_uncovered", "message": reason},
			})
			c.Abort()
			return
		}
		if ex == nil || ex.Text == "" {
			next()
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

		guardCtx, cancel := context.WithTimeout(ctx, time.Duration(svc.RequestBudgetMs())*time.Millisecond)
		defer cancel()

		result, err := svc.Check(guardCtx, &guardrail.CheckRequest{
			Content:   ex.Text,
			Direction: guardrail.DirectionRequest,
			Model:     probe.Model,
			APIKeyID:  apiKeyID,
			TeamID:    teamID,
			OrgID:     orgID,
		})
		if err != nil {
			if svc.IsFailOpen() {
				slog.Warn("guardrail check failed, failing open", "error", err)
				next()
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

		// Partial coverage: the checked content was over the inspection limit
		// and only its prefix was scanned. Surface it — never a silent pass.
		if result != nil && result.Truncated {
			c.Header("X-Guardrail-Coverage", "partial")
			c.Set("guardrail_truncated", true)
			setSecurityEvent(c, result, "partial coverage: content exceeded inspection limit; only the prefix was scanned")
		}

		if result == nil || !result.Blocked {
			if result != nil && (result.Action == "log" || result.Action == "mask") {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", result.RuleName)
				setSecurityEvent(c, result, "")
				if result.Action == "mask" {
					applyRequestMask(c, ex, result)
					streamContent = result.MaskedContent
				}
			}
			next()
			return
		}

		switch result.Action {
		case "block":
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "review":
			// 拦截待人工复核：内容不放行，复核结论只完成审计闭环。
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "log":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result, "")
		case "mask":
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", result.RuleName)
			setSecurityEvent(c, result, "")
			applyRequestMask(c, ex, result)
			streamContent = result.MaskedContent
		}

		next()
	}
}

// applyRequestMask rewrites the request body with the composed mask spans,
// field by field, preserving roles, arrays and non-text (media) blocks.
// Engines that return only a whole-string MaskedContent (no spans) are NOT
// written back: collapsing all user messages into one masked string destroys
// structure. If a mask cannot be applied completely, reject the request.
func applyRequestMask(c *gin.Context, ex *guardrail.ExtractedContent, result *guardrail.CheckResponse) {
	if len(result.MaskedSpans) == 0 {
		slog.Warn("guardrail: mask result without field spans; rejecting request",
			"rule", result.RuleName)
		setSecurityEvent(c, result, "mask not applied: engine returned no field spans")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail mask could not be applied safely"}})
		return
	}
	newBody, allApplied := ex.ApplyMask(result.MaskedSpans)
	if !allApplied || newBody == nil {
		setSecurityEvent(c, result, "mask could not be applied safely; request rejected")
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail mask could not be applied safely"}})
		return
	}
	// Rewrite BOTH the request body and the buffered copy: downstream consumers
	// (cache key, handlers, usage logging) read via GetBodyBytes, which without
	// this sync would keep serving the unmasked original to the upstream.
	// The rewrite is budget-aware: a masked body larger than the original must
	// stay inside the per-request ceiling and the in-flight budget (supervisor
	// F26 review: rewrites may not silently widen memory beyond admission).
	if !SetBodyBytes(c, newBody) {
		c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"type": "request_too_large", "message": "masked request body exceeds the body-size ceiling"},
		})
		return
	}
}

func isGatewayContentPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/admin/api/playground/")
}

func setSecurityEvent(c *gin.Context, result *guardrail.CheckResponse, note string) {
	event := map[string]string{
		"rule":   result.RuleName,
		"action": result.Action,
		"reason": result.Reason,
	}
	if note != "" {
		event["note"] = note
	}
	if result.Truncated {
		event["truncated"] = "true"
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
