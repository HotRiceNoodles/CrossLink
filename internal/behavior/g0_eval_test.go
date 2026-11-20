package behavior

// G0 合成剧本评测（规划冻结 v4.0 §6 门槛 2/3/4 的可执行形态——剧本独立编写，
// 不参与任何参数调整；门槛 1 误报回放需 ≥4 周真实流量，属部署后验收，本地不伪造）。
//
// 方法论：28-30 天稳定基线回填（真实跑 ReaggregateHour/ReaggregateDayHistograms/
// RecomputeBaselines 全链），剧本注入到回填时间线之后的"干净小时"（该小时此前无
// 任何行），保证单因事件不被自身 request_rate 维度稀释。

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

var g0Now = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC) // 固定时钟：H0=10:00(基线内 hour-of-day)

// g0Row 造一行用量（默认正常形态）。
func g0Row(keyID, orgID int64, at time.Time, mut func(*model.UsageLog)) *model.UsageLog {
	r := &model.UsageLog{
		RequestID: "g0-" + at.Format("0102150405.000000000") + "-",
		APIKeyID:  &keyID, OrgID: &orgID, RouteType: "openai",
		ModelRequested: "gpt-x", ModelUsed: "gpt-x",
		InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10,
		Cost: 1.5, BillableCost: 1.5, Currency: "CNY",
		LatencyMs: 200, StatusCode: 200,
		AgentType: "codex", CreatedAt: at,
	}
	if mut != nil {
		mut(r)
	}
	// RequestID 唯一化：追加纳秒+键。
	r.RequestID += r.CreatedAt.Format(".000000000")
	return r
}

// g0SeedEntity 按天回填稳定基线：hours 内每小时 perHour 行。
func g0SeedEntity(t *testing.T, db *gorm.DB, keyID, orgID int64, days int, perHourFor func(day int) int, hours []int) {
	t.Helper()
	end := g0Now.Truncate(time.Hour)
	for d := days; d >= 1; d-- {
		day := end.AddDate(0, 0, -d)
		perHour := perHourFor(days - d) // day 0..days-1
		for _, h := range hours {
			at := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, time.UTC)
			n := perHour
			for i := 0; i < n; i++ {
				row := g0Row(keyID, orgID, at.Add(time.Duration(i)*time.Minute), func(r *model.UsageLog) {
					r.LatencyMs = 180 + i*4
					ft := 40 + i
					r.FirstTokenMs = &ft
				})
				if err := db.Create(row).Error; err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
		}
	}
}

type g0DB = *gorm.DB

// g0Backfill 全链回填：逐小时聚合 + 日直方图 + 全体基线重算。
func g0Backfill(t *testing.T, agg *Aggregator, from, to time.Time) {
	t.Helper()
	ctx := context.Background()
	for h := from.Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		if _, err := agg.ReaggregateHour(ctx, h); err != nil {
			t.Fatalf("backfill hour %s: %v", h, err)
		}
	}
	for d := from.Truncate(24 * time.Hour); d.Before(to); d = d.AddDate(0, 0, 1) {
		if err := agg.ReaggregateDayHistograms(ctx, d); err != nil {
			t.Fatalf("backfill day %s: %v", d, err)
		}
	}
	ents, err := agg.activeEntities(ctx, 40)
	if err != nil {
		t.Fatalf("active entities: %v", err)
	}
	if err := agg.RecomputeBaselines(ctx, ents); err != nil {
		t.Fatalf("recompute: %v", err)
	}
}

// g0InjectAndScore 注入剧本行到干净小时 H0 → 重聚合/重算/打分 → 返回该 key 的台账事件。
func g0InjectAndScore(t *testing.T, agg *Aggregator, db g0DB, keyID, orgID int64, rows int, mut func(i int, r *model.UsageLog)) []model.BehaviorAnomaly {
	t.Helper()
	ctx := context.Background()
	h0 := g0Now.Truncate(time.Hour)
	for i := 0; i < rows; i++ {
		// 秒级铺开（分钟级会让第 60+ 行落进下一小时——rate-10x 剧本实测教训）。
		row := g0Row(keyID, orgID, h0.Add(time.Duration(i)*30*time.Second), func(r *model.UsageLog) {
			if mut != nil {
				mut(i, r)
			}
		})
		if err := db.Create(row).Error; err != nil {
			t.Fatalf("inject: %v", err)
		}
	}
	if _, err := agg.ReaggregateHour(ctx, h0); err != nil {
		t.Fatalf("reaggregate H0: %v", err)
	}
	if err := agg.RecomputeBaselines(ctx, []EntityRef{{Type: EntityTypeKey, ID: keyID}, {Type: EntityTypeOrg, ID: orgID}}); err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if err := agg.ScoreHour(ctx, h0); err != nil {
		t.Fatalf("score H0: %v", err)
	}
	var evs []model.BehaviorAnomaly
	db.Where("entity_type = ? AND entity_id = ?", EntityTypeKey, keyID).Find(&evs)
	return evs
}

func g0Band(evs []model.BehaviorAnomaly) string {
	if len(evs) == 0 {
		return ""
	}
	best := ""
	for _, e := range evs {
		if e.Band == BandCritical {
			return BandCritical
		}
		if e.Band == BandAnomalous {
			best = BandAnomalous
		}
		if best == "" {
			best = e.Band
		}
	}
	return best
}

// TestG0ScenarioDetection 门槛 2：五类合成剧本检出（剧本值独立于调参——10×、
// 全量陌生模型、休眠时段、全错探刺、护栏全触发）。
func TestG0ScenarioDetection(t *testing.T) {
	db := testDB(t)
	agg := &Aggregator{DB: db, Now: func() time.Time { return g0Now }}
	ctx := context.Background()

	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	// key1/org20：28 天稳定 10/小时（无同伴——P 通道不参与，纯自身通道剧本）。
	g0SeedEntity(t, db, 1, 20, 28, func(int) int { return 10 }, hours)
	g0Backfill(t, agg, g0Now.Add(-28*24*time.Hour), g0Now.Truncate(time.Hour))
	_ = ctx

	scenarios := []struct {
		name  string
		keyID int64
		rows  int
		mut   func(i int, r *model.UsageLog)
		want  string // 期望分档（critical/anomalous/suspicious）
	}{
		{"rate-10x", 1, 100, nil, BandCritical},
		{"novel-model", 2, 10, func(_ int, r *model.UsageLog) { r.ModelUsed = "gpt-new"; r.ModelRequested = "gpt-new" }, BandCritical},
		{"dormant-hour", 1, 30, func(i int, r *model.UsageLog) {
			r.CreatedAt = time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		}, BandCritical},
		{"error-probe", 1, 10, func(_ int, r *model.UsageLog) { r.StatusCode = 401 }, BandCritical},
		{"guardrail-spike", 1, 10, func(_ int, r *model.UsageLog) { r.GuardrailTriggered = true; r.GuardrailRule = "rag-kb" }, BandCritical},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			evs := g0InjectAndScore(t, agg, db, sc.keyID, 20, sc.rows, sc.mut)
			band := g0Band(evs)
			dump := ""
			for _, e := range evs {
				if e.Dimensions != nil {
					dump += *e.Dimensions
				}
			}
			if band == "" {
				t.Errorf("%s: NOT detected (no event)", sc.name)
			} else if band != sc.want {
				t.Errorf("%s: band=%s, want %s\ndims=%s", sc.name, band, sc.want, dump)
			}
		})
	}
}

// TestG0ColdStartSafety 门槛 3：新 key 首小时按 org 常态用量运行 → 零事件
// （继承打分 s≈1 → 无维度）；同量级 10× 才应触发（继承通道的检出语义）。
func TestG0ColdStartSafety(t *testing.T) {
	db := testDB(t)
	agg := &Aggregator{DB: db, Now: func() time.Time { return g0Now }}
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	g0SeedEntity(t, db, 1, 10, 28, func(int) int { return 10 }, hours)
	g0Backfill(t, agg, g0Now.Add(-28*24*time.Hour), g0Now.Truncate(time.Hour))

	// 新 key 99：正常用量（=org 常态 10/小时）→ 无事件。
	evs99 := g0InjectAndScore(t, agg, db, 99, 10, 10, nil)
	if len(evs99) != 0 {
		dump := ""
		for _, e := range evs99 {
			if e.Dimensions != nil {
				dump += *e.Dimensions
			}
		}
		t.Fatalf("cold-start normal usage must NOT alert: %d events (gate 3: 新 key 前 14 天误报=0)\ndims=%s", len(evs99), dump)
	}
	// 新 key 98：10× 异常用量 → 继承打分触发（inherited 注记在案）。分档
	// anomalous/critical 皆可——被 org 抑制的 novelty(50)会轻微稀释合成值，
	// G0 门槛要求的是检出，不钉死复合分档。
	evs := g0InjectAndScore(t, agg, db, 98, 10, 100, nil)
	if len(evs) == 0 {
		t.Fatal("cold-start ABNORMAL usage must alert via inherited baseline")
	}
	if band := g0Band(evs); band != BandCritical && band != BandAnomalous {
		t.Fatalf("inherited detection band=%s, want anomalous|critical", band)
	}
	var ev model.BehaviorAnomaly
	db.Where("entity_type = ? AND entity_id = ?", EntityTypeKey, 98).First(&ev)
	if ev.Dimensions == nil || !strings.Contains(*ev.Dimensions, "inherited:org-baseline") {
		t.Fatalf("event must carry inherited note, dims=%v", ev.Dimensions)
	}
}

// TestG0GroomingPeerChannel 门槛 4：30 天渐进 3×（grooming）——自身基线已适应、
// 自身维度全灭，P 通道（org 内 ≥5 稳定同伴的群体对比）必须独立开案。
func TestG0GroomingPeerChannel(t *testing.T) {
	db := testDB(t)
	agg := &Aggregator{DB: db, Now: func() time.Time { return g0Now }}
	hours := []int{8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	// org30：key 1-5 稳定 10/小时 + key 9 grooming 10→30 渐进（30 天）。
	for k := int64(1); k <= 5; k++ {
		g0SeedEntity(t, db, k, 30, 30, func(int) int { return 10 }, hours)
	}
	g0SeedEntity(t, db, 9, 30, 30, func(day int) int { return 10 + 20*day/29 }, hours)
	g0Backfill(t, agg, g0Now.Add(-30*24*time.Hour), g0Now.Truncate(time.Hour))

	// H0：同伴各 10 行（激活 P 通道），grooming key 延续爬坡顶 30 行。
	for k := int64(1); k <= 5; k++ {
		for i := 0; i < 10; i++ {
			if err := db.Create(g0Row(k, 30, g0Now.Truncate(time.Hour).Add(time.Duration(i)*time.Minute), nil)).Error; err != nil {
				t.Fatalf("seed peer row: %v", err)
			}
		}
	}
	evs := g0InjectAndScore(t, agg, db, 9, 30, 30, nil)
	if len(evs) == 0 {
		t.Fatal("grooming (30-day gradual 3x) must be flagged by the peer channel (gate 4)")
	}
	var ev model.BehaviorAnomaly
	db.Where("entity_type = ? AND entity_id = ?", EntityTypeKey, 9).First(&ev)
	if ev.Dimensions == nil || !strings.Contains(*ev.Dimensions, `"peer"`) {
		t.Fatalf("event must carry the peer dimension, dims=%v", ev.Dimensions)
	}
	// 稳定同伴自身不得被 P 通道误伤。
	var peerEvs int64
	db.Model(&model.BehaviorAnomaly{}).
		Where("entity_type = ? AND entity_id IN ?", EntityTypeKey, []int64{1, 2, 3, 4, 5}).Count(&peerEvs)
	if peerEvs != 0 {
		t.Fatalf("stable peers must have zero events, got %d", peerEvs)
	}
}
