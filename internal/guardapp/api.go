package guardapp

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

// registerIngestRoutes 挂载结构化检测 API（设计 DD-2 ①）：/detect 同步
// 裁决、/detect/async 异步 202、/traces/{trace_id}/response 响应向配对、
// /traces/{trace_id}/stream 流式分片（M3，终检语义见 stream.go）。
// 认证 → 两级限速 → handler（幂等在 handler 内、校验之后）。
func (a *App) registerIngestRoutes(r *gin.Engine) {
	g := r.Group("/api/v1", IngestAuth(a.TokenRepo, a.IngestCache), a.Limiter.Middleware())
	g.POST("/detect", a.handleDetect)
	g.POST("/detect/async", a.handleDetectAsync)
	g.POST("/traces/:trace_id/response", a.handleResponse)
	g.POST("/traces/:trace_id/stream", a.handleStreamChunk)
}

// readPayload 读取并校验请求体：大小上限（413）、JSON 解析（400）、协议
// 白名单与 trace_id 约束（400）。direction 缺省取端点语义。
func (a *App) readPayload(c *gin.Context, p *IngestPayload, defaultDirection string) bool {
	limit := a.Cfg.Guard.Ingest.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "read_error", "message": err.Error()}})
		return false
	}
	if int64(len(body)) > limit {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": gin.H{"code": "payload_too_large", "message": "content exceeds ingest body limit"},
		})
		return false
	}
	if err := json.Unmarshal(body, p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "malformed_json", "message": err.Error()}})
		return false
	}
	if p.Protocol == "" {
		p.Protocol = ProtocolGeneric
	}
	switch p.Protocol {
	case ProtocolOpenAI, ProtocolAnthropic, ProtocolGeneric:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_protocol",
			"message": "protocol must be one of openai/anthropic/generic"}})
		return false
	}
	if p.Direction == "" {
		p.Direction = defaultDirection
	}
	if p.TraceID != "" && len(p.TraceID) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_trace_id",
			"message": "trace_id exceeds 64 chars"}})
		return false
	}
	return true
}

// requireTraceID 请求向端点的 trace_id 必填（响应向由路径提供）。
func requireTraceID(c *gin.Context, p *IngestPayload) bool {
	if p.TraceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_trace_id",
			"message": "trace_id is required"}})
		return false
	}
	return true
}

// checkTenant 校验 payload.tenant 与凭证 org 一致（DD-7 租户=org）：接受
// org ID 数字串或 org 名称，未声明放行（org 以凭证为准），不匹配 403。
func (a *App) checkTenant(c *gin.Context, tenant string) bool {
	if tenant == "" {
		return true
	}
	orgID := c.GetInt64("org_id")
	if tenant == strconv.FormatInt(orgID, 10) {
		return true
	}
	var name string
	if err := a.DB.Raw(`SELECT name FROM organizations WHERE id = ?`, orgID).Scan(&name).Error; err == nil && tenant == name {
		return true
	}
	c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "org_mismatch",
		"message": "tenant does not match the ingest token's organization"}})
	return false
}

// seenFirst 幂等闸门：(org_id, trace_id, direction, seq) 重复回放 409——
// 不重跑管线，杜绝重复事件/trace（重复的同步裁决由客户首答承担）。
func (a *App) seenFirst(c *gin.Context, p *IngestPayload) bool {
	if a.Idem.Seen(c.GetInt64("org_id"), p.TraceID, p.Direction, p.Seq) {
		return true
	}
	c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "duplicate",
		"message": "trace already ingested for this direction"}})
	return false
}

func (a *App) detectRequestFrom(c *gin.Context, p *IngestPayload) (*DetectRequest, bool) {
	text, uninspectable, ok := NormalizeContent(p.Content, p.Protocol)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "no_inspectable_content",
			"message": "content carries no inspectable text"}})
		return nil, false
	}
	return &DetectRequest{
		OrgID:         c.GetInt64("org_id"),
		TraceID:       p.TraceID,
		Direction:     p.Direction,
		Text:          text,
		Uninspectable: uninspectable,
		Model:         p.Model,
		Metadata:      MetadataMap(p.Metadata),
	}, true
}

func (a *App) handleDetect(c *gin.Context) {
	var p IngestPayload
	if !a.readPayload(c, &p, string(guardrail.DirectionRequest)) {
		return
	}
	if !requireTraceID(c, &p) || !a.checkTenant(c, p.Tenant) || !a.seenFirst(c, &p) {
		return
	}
	req, ok := a.detectRequestFrom(c, &p)
	if !ok {
		return
	}
	a.writeVerdict(c, p.TraceID, a.Pipeline.CheckSync(c.Request.Context(), req))
}

func (a *App) handleDetectAsync(c *gin.Context) {
	var p IngestPayload
	if !a.readPayload(c, &p, string(guardrail.DirectionRequest)) {
		return
	}
	if !requireTraceID(c, &p) || !a.checkTenant(c, p.Tenant) || !a.seenFirst(c, &p) {
		return
	}
	req, ok := a.detectRequestFrom(c, &p)
	if !ok {
		return
	}
	if !a.Pipeline.SubmitAsync(req) {
		// 队列+溢出打满：丢弃已计数（cl_guard_ingest_dropped_total），
		// 202 如实告知——不反压，客户无需重试（重试同 trace 会 409）
		c.JSON(http.StatusAccepted, gin.H{"trace_id": p.TraceID, "status": "dropped"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"trace_id": p.TraceID, "status": "accepted"})
}

// handleResponse 响应向配对提交：同步返回响应向裁决 + trace 当前状态
// （trace 落库异步，status 可能短暂停留在 open）。
func (a *App) handleResponse(c *gin.Context) {
	traceID := c.Param("trace_id")
	var p IngestPayload
	if !a.readPayload(c, &p, string(guardrail.DirectionResponse)) {
		return
	}
	if p.TraceID != "" && p.TraceID != traceID {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "trace_mismatch",
			"message": "payload trace_id does not match the path"}})
		return
	}
	p.TraceID = traceID
	if !a.checkTenant(c, p.Tenant) || !a.seenFirst(c, &p) {
		return
	}
	req, ok := a.detectRequestFrom(c, &p)
	if !ok {
		return
	}
	res := a.Pipeline.CheckSync(c.Request.Context(), req)
	if res.Verdict == model.GuardVerdictError {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "detection_failed", "message": res.Reason}})
		return
	}
	body := gin.H{
		"trace_id": traceID,
		"verdict":  clientVerdict(res),
		"action":   res.Action,
		"rule":     res.RuleName,
		"reason":   res.Reason,
	}
	if tr, err := a.Traces.Status(c.Request.Context(), c.GetInt64("org_id"), traceID); err == nil {
		body["trace"] = gin.H{"status": tr.Status, "verdict": tr.Verdict}
	}
	c.JSON(http.StatusOK, body)
}

// writeVerdict 同步裁决响应。verdict 是客户决策面语言（allow/block/review，
// 设计 DD-2）——旁路不执行动作，客户可选采纳；action 保留旁路内部语义。
// mask 命中附 masked_spans，由客户自行决定是否应用。
func (a *App) writeVerdict(c *gin.Context, traceID string, res *DetectResult) {
	if res.Verdict == model.GuardVerdictError {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "detection_failed", "message": res.Reason}})
		return
	}
	body := gin.H{
		"trace_id":  traceID,
		"verdict":   clientVerdict(res),
		"action":    res.Action,
		"rule":      res.RuleName,
		"reason":    res.Reason,
		"matched":   res.Matched,
		"truncated": res.Truncated,
	}
	if len(res.Spans) > 0 {
		body["masked_spans"] = res.Spans
	}
	c.JSON(http.StatusOK, body)
}

func clientVerdict(res *DetectResult) string {
	switch {
	case res.Verdict == model.GuardVerdictWouldBlock:
		return "block"
	case res.Action == "review":
		return "review"
	default:
		return "allow" // pass / mask / log
	}
}
