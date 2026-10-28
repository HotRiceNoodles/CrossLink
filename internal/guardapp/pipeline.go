package guardapp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"gorm.io/datatypes"
)

// 检测管线（设计 DD-4/DD-5）：规范化文本进入 guardrail.Check，裁决映射为
// 旁路语义（block→would_block 影子判定），事件与 trace 元数据落库。
//
// 事件落库不走 GuardrailService.AddHandler：emitEvent 用 Background ctx
// 派发且 GuardrailEvent 无 trace 通道，guard_events 的 (trace_id, org_id,
// rule_id, version) 归因只能由管线侧从 CheckResponse.Matched 拿到；且
// handler 路径会把每规则 miss/pass 事件全量落库，对 90 天留存的库表是
// 体量灾难。管线侧只落有信息量的事件：命中归因、执行失败、覆盖缺口。
const (
	defaultDetectBudget = 2000 * time.Millisecond
	previewMaxRunes     = 200 // guard_events.content_preview 摘要长度
	reasonMaxRunes      = 500 // guard_events.reason 长度（列 1000）
	engineGuardService  = "guardrail_service"
)

// DetectRequest 是规范化层输出进入管线的形态（Step 7 API 处理器构造，
// M2 镜像端点复用）。
type DetectRequest struct {
	OrgID         int64
	TraceID       string
	Direction     string // guardrail.DirectionRequest / DirectionResponse
	Text          string
	Uninspectable bool // 结构化报文携带非文本媒体块
	Model         string
	Metadata      map[string]string // payload.metadata → 引擎上下文
}

// DetectResult 是同步路径返回给调用方的裁决。Action 是旁路语义动作
// （would_block/flagged 档的 mask/log/review/pass/error）；Spans 仅 mask
// 命中时有值，同步 API 附带给客户自处理。EngineCtx 是检测执行上下文——
// 引擎（agent 指纹/行为分析等）在 Check 过程写入的产出侧画像随事件落库
// （M3 元数据接线），同步路径响应不带出。
type DetectResult struct {
	Verdict   string // model.GuardVerdict* 常量
	Action    string
	RuleName  string
	Reason    string
	Matched   []guardrail.MatchedRule
	Spans     []guardrail.Span
	Truncated bool
	EngineCtx *guardrail.CheckContext
}

type Pipeline struct {
	svc    *guardrail.GuardrailService
	events *repository.GuardEventRepo
	traces *TraceRecorder
	pool   *worker.Pool
	budget time.Duration
}

func NewPipeline(svc *guardrail.GuardrailService, events *repository.GuardEventRepo, traces *TraceRecorder, pool *worker.Pool, budget time.Duration) *Pipeline {
	if budget <= 0 {
		budget = defaultDetectBudget
	}
	return &Pipeline{svc: svc, events: events, traces: traces, pool: pool, budget: budget}
}

// CheckSync 同步检测：内联执行（受 detect_budget 约束），事件与 trace
// 落库经 worker 池递交——持久化时延不进入客户响应路径。
func (p *Pipeline) CheckSync(ctx context.Context, req *DetectRequest) *DetectResult {
	res := p.detect(ctx, req, "sync")
	p.pool.Submit(func() { p.persist(req, res) })
	return res
}

// SubmitAsync 异步检测：检测与落库整条进 worker 池。队列与溢出额度同时
// 打满即丢弃并计 cl_guard_ingest_dropped_total（DD-4：丢弃+计数，绝不
// 反压客户网关）。accepted=false 表示该 payload 未被检测。
func (p *Pipeline) SubmitAsync(req *DetectRequest) (accepted bool) {
	if !p.pool.TrySubmit(func() {
		res := p.detect(context.Background(), req, "async")
		p.persist(req, res)
	}) {
		ingestDroppedTotal.Inc()
		slog.Warn("guard ingest dropped: detection queue saturated",
			"org_id", req.OrgID, "trace_id", req.TraceID)
		return false
	}
	return true
}

// detect 执行一次检测并映射旁路裁决。mode 仅作指标维度（sync=客户在等、
// async=worker 池），检测路径完全一致。响应向先取回配对 prompt（take-and-
// delete）供上下文引擎消费，与网关 stream_wrapper 同语义。
func (p *Pipeline) detect(ctx context.Context, req *DetectRequest, mode string) (out *DetectResult) {
	start := time.Now()
	defer func() {
		detectDurationSeconds.WithLabelValues(mode).Observe(time.Since(start).Seconds())
		verdictsTotal.WithLabelValues(mode, out.Verdict).Inc()
	}()

	cctx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()

	checkCtx := &guardrail.CheckContext{Metadata: req.Metadata}
	if checkCtx.Metadata == nil {
		checkCtx.Metadata = make(map[string]string)
	}
	cctx = context.WithValue(cctx, guardrail.CtxKeyCheck, checkCtx)

	var prompt string
	if guardrail.Direction(req.Direction) == guardrail.DirectionResponse {
		if pp, ok := p.traces.TakePrompt(req.OrgID, req.TraceID); ok {
			prompt = pp
		}
	}

	res, err := p.svc.Check(cctx, &guardrail.CheckRequest{
		Content:   req.Text,
		Direction: guardrail.Direction(req.Direction),
		Model:     req.Model,
		OrgID:     req.OrgID,
		Prompt:    prompt,
	})

	out = &DetectResult{Verdict: VerdictFromCheck(res, err), Action: bypassAction(res), EngineCtx: checkCtx}
	if err != nil {
		out.Reason = err.Error()
		return out
	}
	if res != nil {
		out.RuleName = res.RuleName
		out.Reason = res.Reason
		out.Matched = res.Matched
		out.Spans = res.MaskedSpans
		out.Truncated = res.Truncated
	}
	return out
}

// persist 在 worker 上下文同步落库：trace 元数据 + 检测事件。失败仅告警
// 不重试——留存是观测面，且检测裁决已在内存得出，丢行不回灌结论。
func (p *Pipeline) persist(req *DetectRequest, res *DetectResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var err error
	if guardrail.Direction(req.Direction) == guardrail.DirectionRequest {
		// 请求向：检测文本存入 TTL 配对窗口供响应向上下文检测
		//（原文仅存内存窗口，DD-5 红线）。
		err = p.traces.RecordRequest(ctx, req.OrgID, req.TraceID, req.Text, res.Verdict)
	} else {
		err = p.traces.RecordResponse(ctx, req.OrgID, req.TraceID, res.Verdict)
	}
	if err != nil {
		slog.Error("guard: record trace", "error", err, "trace_id", req.TraceID, "org_id", req.OrgID)
	}

	rows := p.buildEvents(req, res)
	for i := range rows {
		if err := p.events.Create(ctx, &rows[i]); err != nil {
			slog.Error("guard: persist event", "error", err, "trace_id", req.TraceID, "org_id", req.OrgID)
		}
	}
}

// buildEvents 从裁决与 Matched 归因构造 guard_events 行：
//   - 每个参与命中规则一行（rule_id/version/action 逐规则归因）；
//   - 服务级事件（无规则归因）：执行失败、超长截断（部分覆盖）、非文本
//     块标记、无 Matched 的服务裁决（如 oversize 直接 block）。
//
// pass 无事件——trace 行已记 pass，每规则 miss 落库对留存是体量噪声。
// 红线（DD-5）：preview/reason 经 sanitizePreview 脱敏，原文以 SHA256
// 关联定位。
func (p *Pipeline) buildEvents(req *DetectRequest, res *DetectResult) []model.GuardEvent {
	sha := fmt.Sprintf("%x", sha256.Sum256([]byte(req.Text)))
	preview := sanitizePreview(truncateRunes(req.Text, previewMaxRunes))
	reason := sanitizePreview(truncateRunes(res.Reason, reasonMaxRunes))
	meta := mergedMetadata(req.Metadata, res.EngineCtx)
	var rows []model.GuardEvent

	hit := func(ruleID, ruleVersion int64, engineType, action, severity, reason string) {
		rows = append(rows, model.GuardEvent{
			TraceID: req.TraceID, OrgID: req.OrgID,
			RuleID: ruleID, RuleVersion: ruleVersion, EngineType: engineType,
			Direction: req.Direction, Action: action, Severity: severity,
			Reason: reason, ContentPreview: preview, ContentSHA256: sha,
			Model: req.Model, Metadata: meta, CreatedAt: time.Now(),
		})
	}

	for _, m := range res.Matched {
		hit(m.RuleID, m.Version, "", bypassRuleAction(m.Action), "", reason)
	}
	switch {
	case res.Verdict == model.GuardVerdictError:
		hit(0, 0, engineGuardService, "error", "", reason)
	case res.Verdict == model.GuardVerdictWouldBlock && len(res.Matched) == 0:
		// 服务级 block（如 oversize reject），RuleName 无对应行。
		hit(0, 0, engineGuardService, "would_block", "", reason)
	}
	if res.Truncated {
		hit(0, 0, engineGuardService, "log", "medium", "partial coverage: only the content prefix was inspected")
	}
	if req.Uninspectable && res.Verdict == model.GuardVerdictPass {
		hit(0, 0, engineGuardService, "log", "medium", "uninspectable content parts present (media); text-only inspection")
	}
	return rows
}

// bypassAction 把 CheckResponse 映射为旁路动作：block→would_block 影子
// 判定（事件全量产生，但不影响已放行流量）；mask/log/review 原样。
func bypassAction(res *guardrail.CheckResponse) string {
	if res == nil {
		return "pass"
	}
	if res.Blocked && (res.Action == "" || res.Action == "block") {
		return "would_block"
	}
	if res.Action != "" {
		return res.Action
	}
	return "pass"
}

func bypassRuleAction(action string) string {
	if action == "" || action == "block" {
		return "would_block"
	}
	return action
}

// mergedMetadata 合并 payload metadata 与引擎产出侧画像（M3 元数据接线）：
// 引擎在 Check 过程写入 CheckContext 的字段（agent 指纹 AgentType/RiskLevel、
// 行为分析等经 ctx.Metadata 传递的产出）以 engine_ 前缀落库；键冲突时
// payload 优先——客户申报数据不被引擎产出覆盖。控制台按 engine_* 键展示
// 指纹/行为画像。
func mergedMetadata(payload map[string]string, ctx *guardrail.CheckContext) datatypes.JSON {
	if ctx == nil {
		return metadataJSON(payload)
	}
	merged := make(map[string]string, len(ctx.Metadata)+2)
	for k, v := range ctx.Metadata {
		merged["engine_"+k] = v
	}
	if ctx.AgentType != "" {
		merged["engine_agent_type"] = ctx.AgentType
	}
	if ctx.RiskLevel != "" {
		merged["engine_risk_level"] = ctx.RiskLevel
	}
	for k, v := range payload {
		merged[k] = v
	}
	return metadataJSON(merged)
}

func metadataJSON(m map[string]string) datatypes.JSON {
	if len(m) == 0 {
		return nil // repo Create 落空对象（列 NOT NULL）
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return datatypes.JSON(b)
}

func truncateRunes(s string, n int) string {
	if len(s) <= n { // 字节数不超限则 rune 数必不超限
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// previewPatterns 是 Community 侧的预览脱敏网（DD-5 红线）。商业版
// guardrail.SanitizePreview 复用 credential/PII 内建模式表，guardapp 不
// 依赖 overlay（M1 不动商业仓库），自带紧凑集合覆盖最常见的敏感形态。
// 幂等：脱敏占位符不再命中任何模式。CN_ID 先于 CREDIT_CARD——18 位
// 身份证同时匹配卡号形态，先到先得。
var previewPatterns = []struct {
	re    *regexp.Regexp
	label string
}{
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "EMAIL"},
	{regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`), "IP"},
	{regexp.MustCompile(`\b\d{17}[\dXx]\b`), "CN_ID"},
	{regexp.MustCompile(`\b1[3-9]\d{9}\b`), "PHONE"},
	{regexp.MustCompile(`\b(?:\d[ -]?){13,19}\d\b`), "CREDIT_CARD"},
	{regexp.MustCompile(`\b(?:sk|pk|rk)-[A-Za-z0-9_\-]{16,}\b`), "CREDENTIAL"},
}

func sanitizePreview(text string) string {
	for _, p := range previewPatterns {
		text = p.re.ReplaceAllString(text, "[REDACTED:"+p.label+"]")
	}
	return text
}
