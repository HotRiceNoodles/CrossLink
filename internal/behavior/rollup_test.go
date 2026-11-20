package behavior

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

func TestReaggregateHourAndBaselines(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	hour := base.Truncate(time.Hour)

	mk := func(keyID, orgID int64, created time.Time, status int, upstream *int, gr bool, grRule string, latency int64, firstToken *int, modelUsed string, cacheHit bool) *model.UsageLog {
		return &model.UsageLog{
			RequestID: "req-" + created.Format("150405.000000000") + "-" + modelUsed,
			APIKeyID:  &keyID, OrgID: &orgID, RouteType: "openai",
			ModelRequested: modelUsed, ModelUsed: modelUsed,
			InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10,
			Cost: 1.5, BillableCost: 1.5, Currency: "CNY",
			LatencyMs: int(latency), FirstTokenMs: firstToken,
			StatusCode: status, UpstreamStatus: upstream,
			GuardrailTriggered: gr, GuardrailRule: grRule,
			CacheHit: cacheHit, AgentType: "codex", CreatedAt: created,
		}
	}
	up500 := 500

	// key1：8 条正常 + 1 条网关 4xx + 1 条护栏触发（规则 rag-kb）。
	for i := 0; i < 8; i++ {
		ft := 100 + i
		if err := db.Create(mk(1, 10, hour.Add(time.Duration(i)*time.Minute), 200, nil, false, "", 200+int64(i), &ft, "gpt-x", false)).Error; err != nil {
			t.Fatalf("seed ok row: %v", err)
		}
	}
	if err := db.Create(mk(1, 10, hour.Add(9*time.Minute), 401, nil, false, "", 150, nil, "gpt-x", false)).Error; err != nil {
		t.Fatalf("seed gw error row: %v", err)
	}
	if err := db.Create(mk(1, 10, hour.Add(10*time.Minute), 200, nil, true, "rag-kb", 180, nil, "gpt-y", false)).Error; err != nil {
		t.Fatalf("seed guardrail row: %v", err)
	}
	// key2：2 条 + 1 条上游 5xx。
	for i := 0; i < 2; i++ {
		if err := db.Create(mk(2, 10, hour.Add(time.Duration(i)*time.Minute), 200, nil, false, "", 300, nil, "gpt-x", false)).Error; err != nil {
			t.Fatalf("seed key2 row: %v", err)
		}
	}
	if err := db.Create(mk(2, 10, hour.Add(3*time.Minute), 502, &up500, false, "", 400, nil, "gpt-x", false)).Error; err != nil {
		t.Fatalf("seed upstream error row: %v", err)
	}

	agg := &Aggregator{DB: db}
	if _, err := agg.ReaggregateHour(ctx, hour); err != nil {
		t.Fatalf("ReaggregateHour: %v", err)
	}

	getRl := func(et string, id int64, metric string) model.BehaviorRollup {
		var r model.BehaviorRollup
		if err := db.Where("entity_type=? AND entity_id=? AND metric=? AND bucket_ts=?",
			et, id, metric, hour).First(&r).Error; err != nil {
			t.Fatalf("rollup %s/%d/%s: %v", et, id, metric, err)
		}
		return r
	}

	// A 类计数：key1 10 条请求。
	if r := getRl(EntityTypeKey, 1, MetricRequestRate); r.ValueNum == nil || *r.ValueNum != 10 {
		t.Errorf("key1 request_rate = %v, want 10", r.ValueNum)
	}
	// org10 = 13 条。
	if r := getRl(EntityTypeOrg, 10, MetricRequestRate); r.ValueNum == nil || *r.ValueNum != 13 {
		t.Errorf("org request_rate = %v, want 13", r.ValueNum)
	}
	// C 类分子/分母：key1 err:gw = 1/10。
	r := getRl(EntityTypeKey, 1, MetricErrGw)
	if *r.ValueNum != 1 || *r.ValueDen != 10 {
		t.Errorf("key1 err:gw = %v/%v, want 1/10", *r.ValueNum, *r.ValueDen)
	}
	// key2 err:up = 1/3（UpstreamStatus 非 NULL）。
	r = getRl(EntityTypeKey, 2, MetricErrUp)
	if *r.ValueNum != 1 || *r.ValueDen != 3 {
		t.Errorf("key2 err:up = %v/%v, want 1/3", *r.ValueNum, *r.ValueDen)
	}
	// 护栏触发 = 1/10（只覆盖走完全程的请求，§2.2——被 block 的请求不在 usage_logs）。
	r = getRl(EntityTypeKey, 1, MetricGrTrigger)
	if *r.ValueNum != 1 || *r.ValueDen != 10 {
		t.Errorf("key1 gr:trigger = %v/%v, want 1/10", *r.ValueNum, *r.ValueDen)
	}
	// 目录：key1 model:gpt-x=9、model:gpt-y=1；grule:rag-kb=1。
	if r := getRl(EntityTypeKey, 1, "model:gpt-x"); *r.ValueNum != 9 {
		t.Errorf("model:gpt-x = %v, want 9", *r.ValueNum)
	}
	if r := getRl(EntityTypeKey, 1, "model:gpt-y"); *r.ValueNum != 1 {
		t.Errorf("model:gpt-y = %v, want 1", *r.ValueNum)
	}
	if r := getRl(EntityTypeKey, 1, "grule:rag-kb"); *r.ValueNum != 1 {
		t.Errorf("grule:rag-kb = %v, want 1", *r.ValueNum)
	}
	// 已见集：key1 见过 gpt-x/gpt-y/rag-kb。
	var seenCount int64
	db.Model(&model.BehaviorSeenCategory{}).
		Where("entity_type=? AND entity_id=?", EntityTypeKey, 1).Count(&seenCount)
	if seenCount != 3 {
		t.Errorf("key1 seen categories = %d, want 3", seenCount)
	}

	// 幂等：重跑同一小时，行数不变（upsert 整值覆盖而非追加）。
	if _, err := agg.ReaggregateHour(ctx, hour); err != nil {
		t.Fatalf("re-run ReaggregateHour: %v", err)
	}
	var rlCount int64
	db.Model(&model.BehaviorRollup{}).Where("bucket_ts=?", hour).Count(&rlCount)
	// key1: 9 指标 + 2 model + 1 grule = 12；key2: 9 + 1 model = 10；
	// org10: 9 指标 + model:gpt-x + model:gpt-y + grule:rag-kb = 12。
	if rlCount != 34 {
		t.Errorf("rollup rows after re-run = %d, want 34 (idempotent)", rlCount)
	}

	// B 类日直方图：key1 的 10 行 latency（150-208ms）。
	if err := agg.ReaggregateDayHistograms(ctx, hour); err != nil {
		t.Fatalf("ReaggregateDayHistograms: %v", err)
	}
	var histRow model.BehaviorRollup
	if err := db.Where("entity_type=? AND entity_id=? AND metric=? AND granularity='day'",
		EntityTypeKey, 1, MetricLatency).First(&histRow).Error; err != nil {
		t.Fatalf("latency histogram row: %v", err)
	}
	if histRow.SampleCount != 10 {
		t.Errorf("latency samples = %d, want 10", histRow.SampleCount)
	}
	h, err := ParseLogHistogram(*histRow.ValueHist)
	if err != nil {
		t.Fatalf("parse histogram: %v", err)
	}
	med := h.Median()
	if med < 140 || med > 260 {
		t.Errorf("latency median = %v, want in (140, 260)", med)
	}

	// 基线快照：key1 有 request_rate 小时桶（obs=1）与 latency ALL 桶。
	if err := agg.RecomputeBaselines(ctx, []EntityRef{{Type: EntityTypeKey, ID: 1}}); err != nil {
		t.Fatalf("RecomputeBaselines: %v", err)
	}
	var snap model.BehaviorBaseline
	if err := db.Where("entity_type=? AND entity_id=? AND metric=? AND bucket_key=?",
		EntityTypeKey, 1, MetricRequestRate, fmtHour(hour.Hour())).First(&snap).Error; err != nil {
		t.Fatalf("request_rate snapshot: %v", err)
	}
	if snap.ObsCount != 1 || snap.P50 != 10 {
		t.Errorf("request_rate snapshot obs=%d p50=%v, want 1/10", snap.ObsCount, snap.P50)
	}
	var latSnap model.BehaviorBaseline
	if err := db.Where("entity_type=? AND entity_id=? AND metric=? AND bucket_key='ALL'",
		EntityTypeKey, 1, MetricLatency).First(&latSnap).Error; err != nil {
		t.Fatalf("latency snapshot: %v", err)
	}
	if !floatEps(latSnap.P50, med, 0.01) {
		t.Errorf("latency snapshot p50=%v, want histogram median %v", latSnap.P50, med)
	}
	if latSnap.ModelVersion != modelVersion {
		t.Errorf("snapshot model_version = %d, want %d", latSnap.ModelVersion, modelVersion)
	}
}

// fmtHour 与 computeSnapshots 的 bucket_key 生成一致（fmt.Sprintf("%d", h)，无补零）。
func fmtHour(h int) string { return strconv.Itoa(h) }

func floatEps(a, b, eps float64) bool {
	return math.Abs(a-b) <= eps
}
