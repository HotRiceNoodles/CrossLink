package guardapp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/worker"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// newPipeline 建真实迁移链库 + 一条全局 keyword_filter block 规则 + 管线。
// block 动作用于断言旁路 would_block 影子语义与事件归因。
func newPipeline(t *testing.T, workerCount, queueSize, overflowCap int) (*Pipeline, *worker.Pool, *gorm.DB, int64) {
	t.Helper()
	db, _, _, _ := newTestApp(t)
	rule := guardrail.GuardrailRule{
		Name:      "test-forbidden",
		Type:      "keyword_filter",
		Direction: "both",
		Enabled:   true,
		Severity:  "high",
		Action:    "block",
		Config:    datatypes.JSON([]byte(`{"blocklist":["forbidden-token"]}`)),
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	svc := guardrail.NewGuardrailService(db, nil)
	svc.RefreshSettings(context.Background())
	pool := worker.NewPool("guard-test", workerCount, queueSize, overflowCap)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pool.Shutdown(ctx)
	})
	rec := NewTraceRecorder(repository.NewGuardTraceRepo(db), NewTraceStore(time.Minute, 100))
	p := NewPipeline(svc, repository.NewGuardEventRepo(db), rec, pool, 500*time.Millisecond)
	return p, pool, db, rule.ID
}

// waitFor 轮询等待异步落库（CheckSync/SubmitAsync 的事件持久化经 worker
// 池递交，非同步可见）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func eventCount(t *testing.T, db *gorm.DB, orgID int64, traceID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.GuardEvent{}).
		Where("org_id = ? AND trace_id = ?", orgID, traceID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPipelineSyncBlockPersistsEventAndTrace(t *testing.T) {
	p, _, db, ruleID := newPipeline(t, 2, 8, 0)
	ctx := context.Background()
	text := "hello forbidden-token world"

	res := p.CheckSync(ctx, &DetectRequest{
		OrgID: 1, TraceID: "tr-block", Direction: string(guardrail.DirectionRequest),
		Text: text, Model: "gpt-x", Metadata: map[string]string{"api_key_id": "42"},
	})

	// 旁路影子判定：网关语境的 block 在此是 would_block
	if res.Verdict != model.GuardVerdictWouldBlock || res.Action != "would_block" {
		t.Fatalf("verdict/action: %s/%s", res.Verdict, res.Action)
	}
	if len(res.Matched) != 1 || res.Matched[0].RuleID != ruleID {
		t.Fatalf("matched: %+v want ruleID=%d", res.Matched, ruleID)
	}

	waitFor(t, 2*time.Second, func() bool { return eventCount(t, db, 1, "tr-block") == 1 },
		"one event row expected")

	var ev model.GuardEvent
	if err := db.Where("org_id = ? AND trace_id = ?", 1, "tr-block").First(&ev).Error; err != nil {
		t.Fatal(err)
	}
	if ev.RuleID != ruleID || ev.RuleVersion != 1 {
		t.Fatalf("rule attribution: %+v", ev)
	}
	if ev.Action != "would_block" || ev.Direction != "request" || ev.Model != "gpt-x" {
		t.Fatalf("event mapping: %+v", ev)
	}
	// preview 是检测文本脱敏摘要、sha256 关联定位、metadata 透传非空
	if ev.ContentPreview != text {
		t.Fatalf("preview: %q", ev.ContentPreview)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(text))); ev.ContentSHA256 != want {
		t.Fatalf("sha256 mismatch: %s", ev.ContentSHA256)
	}
	if string(ev.Metadata) == "" || string(ev.Metadata) == "null" {
		t.Fatalf("metadata must be persisted, got %s", ev.Metadata)
	}

	// trace：请求向完成即 open，verdict 已是 would_block
	tr, err := repository.NewGuardTraceRepo(db).FindByOrgTrace(ctx, 1, "tr-block")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != model.GuardTraceOpen || tr.Verdict != model.GuardVerdictWouldBlock || tr.RequestAt == nil {
		t.Fatalf("trace: %+v", tr)
	}
}

func TestPipelinePassStoresNoEvents(t *testing.T) {
	p, _, db, _ := newPipeline(t, 2, 8, 0)
	ctx := context.Background()

	res := p.CheckSync(ctx, &DetectRequest{
		OrgID: 1, TraceID: "tr-pass", Direction: string(guardrail.DirectionRequest),
		Text: "perfectly clean text", Model: "gpt-x",
	})
	if res.Verdict != model.GuardVerdictPass || res.Action != "pass" {
		t.Fatalf("verdict/action: %s/%s", res.Verdict, res.Action)
	}

	// pass 不落事件（trace 行已记 pass）；异步持久化完成后仍应为零
	time.Sleep(100 * time.Millisecond)
	if n := eventCount(t, db, 1, "tr-pass"); n != 0 {
		t.Fatalf("pass must not store events, got %d", n)
	}
	tr, err := repository.NewGuardTraceRepo(db).FindByOrgTrace(ctx, 1, "tr-pass")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Verdict != model.GuardVerdictPass || tr.RequestAt == nil {
		t.Fatalf("trace: %+v", tr)
	}
}

func TestPipelineResponsePairsAndCompletesTrace(t *testing.T) {
	p, _, db, _ := newPipeline(t, 2, 8, 0)
	ctx := context.Background()

	if res := p.CheckSync(ctx, &DetectRequest{
		OrgID: 1, TraceID: "tr-pair", Direction: string(guardrail.DirectionRequest),
		Text: "user asks about forbidden-token", Model: "m",
	}); res.Verdict != model.GuardVerdictWouldBlock {
		t.Fatalf("request verdict: %s", res.Verdict)
	}
	// 响应干净 → verdict 汇总保持 would_block（高档），双向到齐 complete
	if res := p.CheckSync(ctx, &DetectRequest{
		OrgID: 1, TraceID: "tr-pair", Direction: string(guardrail.DirectionResponse),
		Text: "a polite, clean answer", Model: "m",
	}); res.Verdict != model.GuardVerdictPass {
		t.Fatalf("response verdict: %s", res.Verdict)
	}

	waitFor(t, 2*time.Second, func() bool {
		// 请求向事件与 trace 完成分属两个 worker 递交，事件到齐才算落完
		if eventCount(t, db, 1, "tr-pair") != 1 {
			return false
		}
		tr, err := repository.NewGuardTraceRepo(db).FindByOrgTrace(ctx, 1, "tr-pair")
		return err == nil && tr.Status == model.GuardTraceComplete
	}, "trace must complete after both directions")

	tr, _ := repository.NewGuardTraceRepo(db).FindByOrgTrace(ctx, 1, "tr-pair")
	if tr.Verdict != model.GuardVerdictWouldBlock || tr.RequestAt == nil || tr.ResponseAt == nil {
		t.Fatalf("trace: %+v", tr)
	}
	// 响应向 pass 无事件：总事件数保持 1
	if n := eventCount(t, db, 1, "tr-pair"); n != 1 {
		t.Fatalf("events: got %d want 1", n)
	}
}

func TestSubmitAsyncAcceptedPersists(t *testing.T) {
	p, _, db, _ := newPipeline(t, 1, 4, 0)

	if !p.SubmitAsync(&DetectRequest{
		OrgID: 1, TraceID: "tr-async", Direction: string(guardrail.DirectionRequest),
		Text: "async forbidden-token payload", Model: "m",
	}) {
		t.Fatal("submit must be accepted")
	}
	waitFor(t, 2*time.Second, func() bool { return eventCount(t, db, 1, "tr-async") == 1 },
		"async detection must persist its event")
}

func TestSubmitAsyncDropsWhenSaturated(t *testing.T) {
	p, pool, db, _ := newPipeline(t, 1, 1, 0) // 1 worker / 1 slot / 无溢出

	// 占死 worker（确认已进入任务体再继续）+ 填满队列（fillerDone 在 filler
	// 被执行时关闭，用于之后确认池已排空）
	release := make(chan struct{})
	started := make(chan struct{})
	fillerDone := make(chan struct{})
	pool.Submit(func() { close(started); <-release })
	<-started
	pool.Submit(func() { close(fillerDone) })

	before := testutil.ToFloat64(ingestDroppedTotal)
	if p.SubmitAsync(&DetectRequest{
		OrgID: 1, TraceID: "tr-drop", Direction: string(guardrail.DirectionRequest),
		Text: "this will be dropped", Model: "m",
	}) {
		t.Fatal("submit must be rejected when queue and overflow are both full")
	}
	if got := testutil.ToFloat64(ingestDroppedTotal) - before; got != 1 {
		t.Fatalf("dropped counter: got %v want 1", got)
	}
	if n := eventCount(t, db, 1, "tr-drop"); n != 0 {
		t.Fatalf("dropped payload must leave no rows, got %d", n)
	}

	// 释放后队列恢复：filler 执行完即队列空、worker 空闲，递交必成功且
	// 不新增 drop
	close(release)
	<-fillerDone

	if !p.SubmitAsync(&DetectRequest{
		OrgID: 1, TraceID: "tr-after", Direction: string(guardrail.DirectionRequest),
		Text: "clean after recovery", Model: "m",
	}) {
		t.Fatal("submit must succeed after drain")
	}
	if got := testutil.ToFloat64(ingestDroppedTotal) - before; got != 1 {
		t.Fatalf("recovered submit must not increment drops: %v", got)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, err := repository.NewGuardTraceRepo(db).FindByOrgTrace(context.Background(), 1, "tr-after")
		return err == nil
	}, "recovered submit must persist its trace")
}

func TestSanitizePreview(t *testing.T) {
	in := "mail a.b@c.com ip 10.1.2.3 id 11010119900307777X phone 13800138000 card 4111 1111 1111 1111 key sk-abcdefghijklmnopqrst"
	out := sanitizePreview(in)
	for _, secret := range []string{"a.b@c.com", "10.1.2.3", "11010119900307777X", "13800138000", "4111 1111 1111 1111", "sk-abcdefghijklmnopqrst"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %q leaked: %s", secret, out)
		}
	}
	for _, label := range []string{"EMAIL", "IP", "CN_ID", "PHONE", "CREDIT_CARD", "CREDENTIAL"} {
		if !strings.Contains(out, "[REDACTED:"+label+"]") {
			t.Fatalf("label %s missing: %s", label, out)
		}
	}
	// 幂等：占位符不再次命中
	if again := sanitizePreview(out); again != out {
		t.Fatalf("not idempotent:\n%s\n%s", out, again)
	}
	// 非敏感文本原样保留
	if got := sanitizePreview("普通文本 ordinary text 12345"); got != "普通文本 ordinary text 12345" {
		t.Fatalf("plain text altered: %s", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	cjk := "敏感内容摘要测试"
	if got := truncateRunes(cjk, 200); got != cjk {
		t.Fatalf("short string altered: %s", got)
	}
	long := strings.Repeat("密", 300)
	if got := truncateRunes(long, 200); len([]rune(got)) != 200 {
		t.Fatalf("rune truncation: %d", len([]rune(got)))
	}
}

// 指标面：detect 埋点按 mode/verdict 分维度计数（sync 走 CheckSync、async
// 走 SubmitAsync；block 规则 → would_block 影子裁决）。
func TestPipelineMetrics(t *testing.T) {
	p, _, _, _ := newPipeline(t, 2, 8, 0) // 清理由 helper 的 t.Cleanup 负责

	syncBefore := testutil.ToFloat64(verdictsTotal.WithLabelValues("sync", model.GuardVerdictWouldBlock))
	asyncBefore := testutil.ToFloat64(verdictsTotal.WithLabelValues("async", model.GuardVerdictPass))

	res := p.CheckSync(context.Background(), &DetectRequest{
		OrgID: 1, TraceID: "metric-sync", Direction: "request", Text: "has forbidden-token"})
	if res.Verdict != model.GuardVerdictWouldBlock {
		t.Fatalf("sync verdict: %s", res.Verdict)
	}
	if got := testutil.ToFloat64(verdictsTotal.WithLabelValues("sync", model.GuardVerdictWouldBlock)) - syncBefore; got != 1 {
		t.Fatalf("sync would_block delta: %v", got)
	}

	if !p.SubmitAsync(&DetectRequest{
		OrgID: 1, TraceID: "metric-async", Direction: "request", Text: "clean text"}) {
		t.Fatal("async submit must be accepted")
	}
	waitFor(t, 2*time.Second, func() bool {
		return testutil.ToFloat64(verdictsTotal.WithLabelValues("async", model.GuardVerdictPass))-asyncBefore == 1
	}, "async pass verdict must be counted")
}
