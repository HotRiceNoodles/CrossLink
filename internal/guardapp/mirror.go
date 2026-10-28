package guardapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
)

// 原生 HTTP 镜像接收端（设计 DD-2 ②，M2）：配合 Nginx mirror /
// APISIX proxy-mirror / Envoy tap 等网络层镜像机制回放原始报文。
// 与结构化 API 共用认证、限速、幂等与检测管线，仅入口形态不同：
//   - 请求向：/mirror/v1/chat/completions、/mirror/v1/messages 直接收
//     完整协议报文，复用 NormalizeContent 的协议体抽取路径；
//   - 响应向：/mirror/traces/{trace_id}/response 收完整 JSON 响应体，
//     或 SSE 全量捕获文本（Content-Type: text/event-stream 走终检重组）；
//   - 镜像流量是 fire-and-forget：不做同步裁决，响应仅 {accepted, trace_id}，
//     重复回放回 202 duplicate:true 而非 409（网络层重试不感知错误语义）。
const (
	mirrorTraceHeader = "X-Guard-Trace"
	requestIDHeader   = "X-Request-Id"
	mirrorTracePrefix = "m-" // 生成型 trace 前缀（区别于客户自报 ID）
)

// registerMirrorRoutes 挂载镜像组：认证与两级限速与结构化 API 同链。
//
// 路径别名（/v1/chat/completions、/v1/messages 原路径）：Envoy 的
// request_mirror_policies 无法对影子请求单独改写路径——主集群与影子集群
// 拿到同一 path，纯配置档只能在 guard 侧按原始路径接收。别名与 /mirror/*
// 行为完全一致（同 handler，ingest 认证必需）；guard 无其他 /v1 路由，
// 不存在与真实端点的冲突。
func (a *App) registerMirrorRoutes(r *gin.Engine) {
	g := r.Group("/mirror", IngestAuth(a.TokenRepo, a.IngestCache), a.Limiter.Middleware())
	g.POST("/v1/chat/completions", a.mirrorHandler(ProtocolOpenAI))
	g.POST("/v1/messages", a.mirrorHandler(ProtocolAnthropic))
	g.POST("/traces/:trace_id/response", a.handleMirrorResponse)

	alias := r.Group("/v1", IngestAuth(a.TokenRepo, a.IngestCache), a.Limiter.Middleware())
	alias.POST("/chat/completions", a.mirrorHandler(ProtocolOpenAI))
	alias.POST("/messages", a.mirrorHandler(ProtocolAnthropic))
}

// readMirrorBody 读镜像报文体并按 Content-Encoding 解压：gzip 解压后重检
// 上限（压缩比攻击防护）；其余编码 415（开放问题 6 的 M2 取舍：只解 gzip
// ——nginx mirror 透传客户原编码，gzip 是网关链路事实标准；br/zstd 场景
// 由客户侧去掉编码头再镜像）。方向仅影响拒绝事件的归因字段。
func (a *App) readMirrorBody(c *gin.Context, direction string) ([]byte, bool) {
	limit := a.Cfg.Guard.Ingest.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	reject := func(status int, code, message string) {
		a.mirrorReject(c, "", direction, status, code, message)
	}
	read := func() ([]byte, bool) {
		b, err := io.ReadAll(io.LimitReader(c.Request.Body, limit+1))
		if err != nil {
			reject(http.StatusBadRequest, "read_error", err.Error())
			return nil, false
		}
		if int64(len(b)) > limit {
			reject(http.StatusRequestEntityTooLarge, "payload_too_large",
				"content exceeds ingest body limit")
			return nil, false
		}
		return b, true
	}
	switch strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Encoding"))) {
	case "", "identity":
		return read()
	case "gzip":
		body, ok := read()
		if !ok {
			return nil, false
		}
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			reject(http.StatusUnsupportedMediaType, "bad_gzip", err.Error())
			return nil, false
		}
		defer zr.Close()
		dec, err := io.ReadAll(io.LimitReader(zr, limit+1))
		if err != nil {
			reject(http.StatusUnsupportedMediaType, "bad_gzip", err.Error())
			return nil, false
		}
		if int64(len(dec)) > limit {
			reject(http.StatusRequestEntityTooLarge, "payload_too_large",
				"decompressed content exceeds ingest body limit")
			return nil, false
		}
		return dec, true
	default:
		reject(http.StatusUnsupportedMediaType, "unsupported_encoding",
			"only identity and gzip bodies are accepted")
		return nil, false
	}
}

// mirrorTraceID 取回放 trace：客户报文带 x-request-id / x-guard-trace 则
// 沿用（响应提交须回带同一 trace），否则生成。客户 ID 只接受保守字符集
// ——trace 落库为字符串列，不接受任意字节。
func mirrorTraceID(c *gin.Context) string {
	id := c.GetHeader(requestIDHeader)
	if id == "" {
		id = c.GetHeader(mirrorTraceHeader)
	}
	if id != "" && len(id) <= 64 {
		valid := true
		for _, r := range id {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '-', r == '_', r == '.', r == ':':
			default:
				valid = false
			}
		}
		if valid {
			return id
		}
	}
	return genMirrorTraceID()
}

func genMirrorTraceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return mirrorTracePrefix + "fallback"
	}
	return mirrorTracePrefix + hex.EncodeToString(b[:])
}

// mirrorHandler 请求向回放：协议由端点决定（chat/completions=openai、
// messages=anthropic），报文即完整协议请求体。
func (a *App) mirrorHandler(protocol string) gin.HandlerFunc {
	direction := string(guardrail.DirectionRequest)
	return func(c *gin.Context) {
		body, ok := a.readMirrorBody(c, direction)
		if !ok {
			return
		}
		traceID := mirrorTraceID(c)
		c.Header(mirrorTraceHeader, traceID)

		// 幂等：客户自报 trace 的重复回放不重跑管线（fire-and-forget 语义
		// 回 202 duplicate，不回 409）；生成型 trace 全新，不会重复。
		// Seen 首次=true：false 即重复回放。
		if !strings.HasPrefix(traceID, mirrorTracePrefix) &&
			!a.Idem.Seen(c.GetInt64("org_id"), traceID, direction, 0) {
			mirrorIngestTotal.WithLabelValues("duplicate").Inc()
			c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID, "duplicate": true})
			return
		}

		text, uninspectable, ok := NormalizeContent(body, protocol)
		if !ok {
			// 畸形报文记事件（无检测文本的覆盖缺口），但不向网络层镜像端
			// 回错误——重试同样畸形，只制造噪声。
			a.recordMirrorReject(c, traceID, direction, "malformed_payload", "mirror replay carries no inspectable text")
			c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID})
			return
		}
		req := &DetectRequest{
			OrgID:         c.GetInt64("org_id"),
			TraceID:       traceID,
			Direction:     direction,
			Text:          text,
			Uninspectable: uninspectable,
			Model:         mirrorModel(body),
		}
		if !a.Pipeline.SubmitAsync(req) {
			mirrorIngestTotal.WithLabelValues("dropped").Inc()
			c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID, "dropped": true})
			return
		}
		mirrorIngestTotal.WithLabelValues("accepted").Inc()
		c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID})
	}
}

// handleMirrorResponse 响应向回放：完整 JSON 响应体（openai/anthropic
// 协议体、纯字符串、content 字段对象均可——走 NormalizeContent 自动分派）
// 或 SSE 全量捕获文本（Content-Type: text/event-stream，终检重组）。
func (a *App) handleMirrorResponse(c *gin.Context) {
	direction := string(guardrail.DirectionResponse)
	traceID := c.Param("trace_id")
	if len(traceID) > 64 {
		a.mirrorReject(c, traceID, direction, http.StatusBadRequest, "invalid_trace_id", "trace_id exceeds 64 chars")
		return
	}
	body, ok := a.readMirrorBody(c, direction)
	if !ok {
		return
	}
	c.Header(mirrorTraceHeader, traceID)

	var text string
	var uninspectable bool
	if strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "text/event-stream") {
		text = reassembleSSE(body)
		if text == "" {
			a.recordMirrorReject(c, traceID, direction, "malformed_payload", "SSE replay carries no text deltas")
			c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID})
			return
		}
	} else if t, u, k := mirrorResponseText(body); k {
		// 响应形态优先：openai choices[].message.content / anthropic 顶层
		// content blocks——共享抽取器只覆盖请求形态（网关响应检查不走
		// HTTP 报文抽取），响应回放不能复用它。
		text, uninspectable = t, u
	} else {
		// 兜底：纯字符串 / {"content":...} / 请求形态协议体（客户回放
		// 请求形日志的场景）。
		text, uninspectable, ok = NormalizeContent(body, ProtocolOpenAI)
		if !ok {
			text, uninspectable, ok = NormalizeContent(body, ProtocolAnthropic)
		}
		if !ok {
			a.recordMirrorReject(c, traceID, direction, "malformed_payload", "response replay carries no inspectable text")
			c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID})
			return
		}
	}

	// Seen 首次=true：false 即重复回放，不重跑管线。
	if !a.Idem.Seen(c.GetInt64("org_id"), traceID, direction, 0) {
		mirrorIngestTotal.WithLabelValues("duplicate").Inc()
		c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID, "duplicate": true})
		return
	}
	req := &DetectRequest{
		OrgID:         c.GetInt64("org_id"),
		TraceID:       traceID,
		Direction:     direction,
		Text:          text,
		Uninspectable: uninspectable,
		Model:         mirrorModel(body),
	}
	if !a.Pipeline.SubmitAsync(req) {
		mirrorIngestTotal.WithLabelValues("dropped").Inc()
		c.JSON(http.StatusAccepted, gin.H{"accepted": false, "trace_id": traceID, "dropped": true})
		return
	}
	mirrorIngestTotal.WithLabelValues("accepted").Inc()
	c.JSON(http.StatusAccepted, gin.H{"accepted": true, "trace_id": traceID})
}

// reassembleSSE 终检重组：扫 data: 行，openai（choices[].delta.content）与
// anthropic（content_block_delta.delta.text）两种分片形态都尝试，文本增量
// 按到达序拼接。畸形行跳过（镜像捕获可能截断在任意边界），[DONE] 终止。
func reassembleSSE(body []byte) string {
	var sb strings.Builder
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var anthropic struct {
			Type  string `json:"type"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &anthropic) == nil && anthropic.Type != "" {
			// anthropic 事件先判：openai 结构体对无 choices 的事件也解析
			// 成功，先走 openai 分支会把 anthropic 分片静默吞掉。
			if anthropic.Type == "content_block_delta" && anthropic.Delta.Text != "" {
				sb.WriteString(anthropic.Delta.Text)
			}
			continue
		}
		var openai struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &openai) == nil {
			for _, ch := range openai.Choices {
				sb.WriteString(ch.Delta.Content)
			}
		}
	}
	return sb.String()
}

// mirrorModel 从回放报文取 model 字段（两种协议同名）。取不到留空——
// 引擎按空模型处理（与结构化 API 一致）。
func mirrorModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Model
}

// mirrorResponseText 从完整 JSON 响应体抽取终检文本，覆盖两种协议的非流
// 式响应形态（共享 ExtractContent 只认请求形态——网关的响应向检查在
// handler 内拿组装完的内容，不经 HTTP 报文抽取，镜像回放没有这层）：
//   - openai：choices[].message.content（字符串或 text parts 数组）；
//   - anthropic：顶层 content blocks（text 块 + tool_result 内嵌文本）。
//
// ok=false 表示不是这两种形态（调用方走兜底路径）。媒体块置 uninspectable
// （与网关 strict coverage 同语义）。
func mirrorResponseText(body []byte) (text string, uninspectable bool, ok bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return "", false, false
	}
	var sb strings.Builder
	if rawChoices, has := root["choices"]; has {
		var choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(rawChoices, &choices) != nil {
			return "", false, false
		}
		for _, ch := range choices {
			s, u := textFromParts(ch.Message.Content)
			sb.WriteString(s)
			uninspectable = uninspectable || u
		}
		return sb.String(), uninspectable, sb.Len() > 0 || uninspectable
	}
	if rawContent, has := root["content"]; has {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(rawContent, &blocks) != nil {
			return "", false, false
		}
		for _, b := range blocks {
			t, _ := guardrail.RawString(b["type"])
			switch t {
			case "text":
				if s, isStr := guardrail.RawString(b["text"]); isStr {
					sb.WriteString(s)
				}
			case "tool_result":
				s, u := textFromParts(b["content"])
				sb.WriteString(s)
				uninspectable = uninspectable || u
			default:
				if t != "" {
					uninspectable = true // image/document 等非文本块
				}
			}
		}
		return sb.String(), uninspectable, sb.Len() > 0 || uninspectable
	}
	return "", false, false
}

// textFromParts 取 OpenAI 风格 content 值的文本：字符串直取，text parts
// 数组逐块拼接；非文本 part 置 uninspectable。
func textFromParts(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if s, isStr := guardrail.RawString(raw); isStr {
		return s, false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return "", false
	}
	var sb strings.Builder
	uninspectable := false
	for _, p := range parts {
		t, _ := guardrail.RawString(p["type"])
		if t != "text" {
			if t != "" {
				uninspectable = true
			}
			continue
		}
		if s, isStr := guardrail.RawString(p["text"]); isStr {
			sb.WriteString(s)
		}
	}
	return sb.String(), uninspectable
}

// mirrorReject 是镜像入口的硬拒绝（413/415/400）：计指标、记事件（DD-10
// 超限计事件——拒绝时可能尚无 trace，事件以空 trace_id 落库归因 org）并
// 回错误——这类拒绝客户端可修复（去编码/减体积），值得明确回错而非静默
// 202。
func (a *App) mirrorReject(c *gin.Context, traceID, direction string, status int, code, message string) {
	mirrorIngestTotal.WithLabelValues("rejected").Inc()
	a.recordMirrorReject(c, traceID, direction, code, message)
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// recordMirrorReject 落一条覆盖缺口事件（action=log）：镜像流量被拒绝
// 意味着这段流量未被检测——与队列打满的 dropped 同属"有流量未被检测"
// 观测面。同步落库（低频路径，不值得再进 worker 池），事件 repo 复用
// 管线实例（同包私有字段直取，不为低频路径在 App 上加字段）。
func (a *App) recordMirrorReject(c *gin.Context, traceID, direction, code, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := a.Pipeline.events.Create(ctx, &model.GuardEvent{
		TraceID:    traceID,
		OrgID:      c.GetInt64("org_id"),
		EngineType: engineGuardService,
		Direction:  direction,
		Action:     "log",
		Severity:   "medium",
		Reason:     sanitizePreview(truncateRunes("mirror ingest rejected ("+code+"): "+message, reasonMaxRunes)),
		CreatedAt:  time.Now(),
	})
	if err != nil {
		slog.Warn("guard: persist mirror-reject event", "error", err, "trace_id", traceID)
	}
}
