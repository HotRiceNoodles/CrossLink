package middleware

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

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

const maxGuardrailBodySize = 10 << 20

// GuardrailsRequest guards request-direction content. toolObs (agent toolset
// seam, nil in Community) receives a Declared-only observation when a request
// is blocked or held for review — the blocked agent's tool declarations are
// capability evidence worth profiling even though no call was answered.
func GuardrailsRequest(svc *guardrail.GuardrailService, toolObs domain.ToolObserver) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("X-CrossLink-Model-Review") != "" {
			c.AbortWithStatusJSON(400, gin.H{"error": "审核服务地址不能指向网关入口"})
			return
		}
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
		switch c.Request.URL.Path {
		case "/admin/api/playground/stream":
			probe.Stream = true
		case "/admin/api/playground/chat":
			probe.Stream = false
		}

		originalPrompt := originalTurnPrompt(bodyBytes)
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
				meta := guardrail.CheckRequest{Content: streamContent, Prompt: originalPrompt, Model: probe.Model, Direction: guardrail.DirectionResponse, OrgID: c.GetInt64("org_id")}
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
		// org_id(M3):kb_poisoning 指纹库按 org 维度存取;缺省 0 = 全局
		// 域(无组织上下文时指纹退化为全局匹配,语义见冻结件 §5.3)。
		if orgID > 0 {
			checkCtx.Metadata["org_id"] = fmt.Sprintf("%d", orgID)
		}
		ctx := context.WithValue(c.Request.Context(), guardrail.CtxKeyCheck, checkCtx)
		ctx = context.WithValue(ctx, guardrail.CtxKeyUserAgent, c.GetHeader("User-Agent"))
		ctx = context.WithValue(ctx, guardrail.CtxKeyHeaders, c.Request.Header)
		// kb_poisoning zw-v2 plumbing: protocol-segment zones in e.Text
		// coordinates (M1 C 线); engines read them from the check context.
		ctx = guardrail.WithSegmentZones(ctx, ex.ZoneMap())
		c.Request = c.Request.WithContext(ctx)

		guardCtx, cancel := context.WithTimeout(ctx, time.Duration(svc.RequestBudgetMs())*time.Millisecond)
		defer cancel()

		result, err := svc.Check(guardCtx, &guardrail.CheckRequest{
			Input:     ex.DetectionInput(),
			Content:   ex.Text,
			Direction: guardrail.DirectionRequest,
			Model:     probe.Model,
			APIKeyID:  apiKeyID,
			TeamID:    teamID,
			OrgID:     orgID,
		})
		if err != nil {
			// kb enforce 失败关闭(P1-5,冻结件 C13/R12):类型化错误越过全局
			// fail-open——强制模式规则故障不得放行待检请求。
			if errors.Is(err, guardrail.ErrKbEnforceFailClosed) {
				slog.Error("guardrail: kb_poisoning enforce failure, failing closed", "error", err)
				c.JSON(http.StatusServiceUnavailable, gin.H{
					"error": gin.H{"type": "guardrail_error", "message": "kb poisoning enforcement unavailable (fail-closed)"},
				})
				c.Abort()
				return
			}
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

		// applyRequestSanitize:冻结件 C3 的 sanitize 机制(社区层)。改写复用
		// mask 通道(C1 共享 span);改写后对消毒正文**复检**(共享 guardCtx
		// 预算)。复检仍需阻断/再次 sanitize → 升级 block(级联上限由组合层
		// 控制,机制层单轮);复检命中 mask → 按 mask 语义补一轮改写;任一
		// 步失败 fail-closed。返回消毒(可能再 mask)后的检测文本供流式护栏。
		applyRequestSanitize := func(result *guardrail.CheckResponse) string {
			if len(result.MaskedSpans) == 0 {
				setSecurityEvent(c, result, "sanitize not applied: engine returned no field spans")
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail sanitize could not be applied safely"}})
				return ""
			}
			newBody, allApplied := ex.ApplyMask(result.MaskedSpans)
			if !allApplied || newBody == nil {
				setSecurityEvent(c, result, "sanitize could not be applied safely; request rejected")
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "guardrail_blocked", "message": "guardrail sanitize could not be applied safely"}})
				return ""
			}
			if !SetBodyBytes(c, newBody) {
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
					"error": gin.H{"type": "request_too_large", "message": "sanitized request body exceeds the body-size ceiling"},
				})
				return ""
			}
			// 复检:对消毒后正文重新提取 + 全量检查(同一 deadline,复检开
			// 销计入请求预算)。
			ex2 := guardrail.ExtractContent(newBody, c.Request.URL.Path)
			sanitized := ""
			if ex2 != nil {
				sanitized = ex2.Text
			}
			if ex2 != nil && ex2.Text != "" {
				re, err := svc.Check(guardCtx, &guardrail.CheckRequest{
					Content:   ex2.Text,
					Direction: guardrail.DirectionRequest,
					Model:     probe.Model,
					APIKeyID:  apiKeyID,
					TeamID:    teamID,
					OrgID:     orgID,
				})
				if err != nil {
					// enforce 路径 fail-closed(冻结件 C13/R12):检查失败
					// 不得放行已处置请求。
					setSecurityEvent(c, result, "sanitize re-check failed; request rejected")
					writeGuardrailError(c, result, c.Request.URL.Path)
					c.Abort()
					return ""
				}
				if re != nil && re.Blocked {
					setSecurityEvent(c, re, "sanitize re-check blocked; escalating")
					writeGuardrailError(c, re, c.Request.URL.Path)
					c.Abort()
					return ""
				}
				if re != nil && re.Action == "sanitize" {
					setSecurityEvent(c, re, "sanitize re-check requires another pass; escalating to block")
					writeGuardrailError(c, re, c.Request.URL.Path)
					c.Abort()
					return ""
				}
				if re != nil && re.Action == "mask" && len(re.MaskedSpans) > 0 {
					applyRequestMask(c, ex2, re)
					sanitized = re.MaskedContent
				}
			}
			c.Header("X-Guardrail-Action", "sanitize")
			setSecurityEvent(c, result, "sanitized: content rewritten and re-checked")
			return sanitized
		}

		if result == nil || !result.Blocked {
			if result != nil && (result.Action == "log" || result.Action == "mask" || result.Action == "sanitize") {
				c.Set("guardrail_triggered", true)
				c.Set("guardrail_rule", result.RuleName)
				setSecurityEvent(c, result, "")
				switch result.Action {
				case "mask":
					applyRequestMask(c, ex, result)
					streamContent = result.MaskedContent
				case "sanitize":
					streamContent = applyRequestSanitize(result)
				}
			}
			next()
			return
		}

		// Declared-only toolset observation for the blocked request. No
		// invocations exist (nothing was answered), but the declared tools
		// are the key's real capability claim — that is the security signal.
		observeBlocked := func() {
			if toolObs == nil || apiKeyID <= 0 {
				return
			}
			declared := parseDeclaredToolsFromRaw(blockedToolProtocol(c), bodyBytes)
			if len(declared) == 0 {
				return
			}
			toolObs.Observe(domain.ToolObservation{
				APIKeyID: apiKeyID,
				TeamID:   teamID,
				OrgID:    orgID,
				Source:   "proxy",
				Protocol: blockedToolProtocol(c),
				Model:    probe.Model,
				Declared: declared,
			})
		}

		switch result.Action {
		case "block":
			observeBlocked()
			writeGuardrailError(c, result, c.Request.URL.Path)
			c.Abort()
			return
		case "review":
			// 拦截待人工复核：内容不放行，复核结论只完成审计闭环。
			observeBlocked()
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
