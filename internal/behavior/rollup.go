package behavior

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// 聚合作业（规划冻结 v4.0 §5.3/§5.5）。热路径纪律（D6）：本文件全部逻辑运行在
// 后台作业里，代理链路零接触。时间桶边界由 Go 侧计算（D7/D9），SQL 只做
// WHERE 范围 + GROUP BY 实体列——不含任何方言时间函数。
//
// 指标命名（§5.5 规范表）：
//
//	A 类（hour）: request_rate / tokens:in / tokens:out / tokens:reasoning / cost:CNY / cost:USD
//	C 类（hour）: err:gw / err:up / gr:trigger（value_num=分子，value_den=该桶请求数）
//	B 类（day） : latency / first_token（value_hist=log₂ 直方图，D11）
//	N 类（hour）: model:{name} / grule:{name}（计数，供新颖度与展示）

const (
	EntityTypeKey = "key"
	EntityTypeOrg = "org"

	MetricRequestRate = "request_rate"
	MetricTokensIn    = "tokens:in"
	MetricTokensOut   = "tokens:out"
	MetricTokensRea   = "tokens:reasoning"
	MetricCostCNY     = "cost:CNY"
	MetricCostUSD     = "cost:USD"
	MetricErrGw       = "err:gw"
	MetricErrUp       = "err:up"
	MetricGrTrigger   = "gr:trigger"
	MetricLatency     = "latency"
	MetricFirstToken  = "first_token"

	reaggregateLookback = 48 * time.Hour // 迟到数据幂等重聚合窗口（§5.2）
	baselineWindowDays  = 28
	// modelVersion：附录 A 公式版本。v1=冻结 v4.0 原始实现（未部署）；v2=G0
	// 剧本集通过时的运行化定案（M4 补注：signalFloor 噪声地板 / P 通道独立
	// 维度 / 冷启动 established 判定）。
	modelVersion = 2
)

// Aggregator 是行为基线聚合作业。
type Aggregator struct {
	DB  *gorm.DB
	Now func() time.Time
}

func (a *Aggregator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// entityAgg 是一条 GROUP BY 结果行（key 或 org 同构）。
type entityAgg struct {
	EntityID int64
	Requests int64
	TokIn    int64
	TokOut   int64
	TokRea   int64
	CostCNY  float64
	CostUSD  float64
	ErrGw    int64
	ErrUp    int64
	GrTrig   int64
}

// hourEntityQuery 按 entityType 选择分组列，返回聚合行。
func (a *Aggregator) hourEntityQuery(ctx context.Context, from, to time.Time, entityType string) ([]entityAgg, error) {
	col := "api_key_id"
	where := "api_key_id IS NOT NULL"
	if entityType == EntityTypeOrg {
		col = "org_id"
		where = "org_id IS NOT NULL"
	}
	sql := fmt.Sprintf(`
SELECT %[1]s AS entity_id,
       COUNT(*) AS requests,
       COALESCE(SUM(input_tokens), 0)      AS tok_in,
       COALESCE(SUM(output_tokens), 0)     AS tok_out,
       COALESCE(SUM(reasoning_tokens), 0)  AS tok_rea,
       COALESCE(SUM(CASE WHEN currency = 'CNY' THEN billable_cost ELSE 0 END), 0) AS cost_cny,
       COALESCE(SUM(CASE WHEN currency = 'USD' THEN billable_cost ELSE 0 END), 0) AS cost_usd,
       SUM(CASE WHEN status_code >= 400 AND upstream_status IS NULL THEN 1 ELSE 0 END)      AS err_gw,
       SUM(CASE WHEN status_code >= 400 AND upstream_status IS NOT NULL THEN 1 ELSE 0 END)  AS err_up,
       SUM(CASE WHEN guardrail_triggered THEN 1 ELSE 0 END)                                  AS gr_trig
FROM usage_logs
WHERE created_at >= ? AND created_at < ? AND %[2]s
GROUP BY %[1]s`, col, where)
	var rows []entityAgg
	if err := raw(a.DB, ctx, sql, from.UTC(), to.UTC()).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("aggregate %s %s: %w", entityType, from.Format(time.RFC3339), err)
	}
	return rows, nil
}

// ReaggregateHour 重聚合一个小时桶（幂等：每桶从自己的小时窗口从头计算，upsert 整值覆盖）。
// 返回该小时有请求的实体集合（受影响实体，供快照重算）。
func (a *Aggregator) ReaggregateHour(ctx context.Context, hourStart time.Time) ([]model.BehaviorSeenCategory, error) {
	from, to := hourStart.Truncate(time.Hour), hourStart.Truncate(time.Hour).Add(time.Hour)
	var rollups []model.BehaviorRollup
	var seen []model.BehaviorSeenCategory
	hour := from.UTC()

	for _, et := range []string{EntityTypeKey, EntityTypeOrg} {
		aggs, err := a.hourEntityQuery(ctx, from, to, et)
		if err != nil {
			return nil, err
		}
		for _, g := range aggs {
			req := float64(g.Requests)
			num := func(v float64) *float64 { return &v }
			rollups = append(rollups,
				rl(et, g.EntityID, MetricRequestRate, "hour", hour, num(req), nil, g.Requests),
				rl(et, g.EntityID, MetricTokensIn, "hour", hour, num(float64(g.TokIn)), nil, g.Requests),
				rl(et, g.EntityID, MetricTokensOut, "hour", hour, num(float64(g.TokOut)), nil, g.Requests),
				rl(et, g.EntityID, MetricTokensRea, "hour", hour, num(float64(g.TokRea)), nil, g.Requests),
				rl(et, g.EntityID, MetricCostCNY, "hour", hour, num(g.CostCNY), nil, g.Requests),
				rl(et, g.EntityID, MetricCostUSD, "hour", hour, num(g.CostUSD), nil, g.Requests),
				rl(et, g.EntityID, MetricErrGw, "hour", hour, num(float64(g.ErrGw)), &req, g.Requests),
				rl(et, g.EntityID, MetricErrUp, "hour", hour, num(float64(g.ErrUp)), &req, g.Requests),
				rl(et, g.EntityID, MetricGrTrigger, "hour", hour, num(float64(g.GrTrig)), &req, g.Requests),
			)
		}
		// N 类目录：模型与护栏规则（§5.10——计数桶供体量展示；新颖度判定读已见集）。
		cats, catRollups, err := a.hourCategories(ctx, from, to, et, hour)
		if err != nil {
			return nil, err
		}
		rollups = append(rollups, catRollups...)
		seen = append(seen, cats...)
	}
	if err := UpsertRollups(ctx, a.DB, rollups); err != nil {
		return nil, err
	}
	if err := UpsertSeenCategories(ctx, a.DB, seen); err != nil {
		return nil, err
	}
	return seen, nil
}

func rl(et string, id int64, metric, gran string, ts time.Time, num, den *float64, samples int64) model.BehaviorRollup {
	return model.BehaviorRollup{
		EntityType: et, EntityID: id, Metric: metric, Granularity: gran,
		BucketTs: ts, ValueNum: num, ValueDen: den, SampleCount: samples,
	}
}

// hourCategories 聚合该小时的 model_used / guardrail_rule 目录计数与已见集 upsert 行。
func (a *Aggregator) hourCategories(ctx context.Context, from, to time.Time, entityType string, hour time.Time) ([]model.BehaviorSeenCategory, []model.BehaviorRollup, error) {
	col, where := "api_key_id", "api_key_id IS NOT NULL"
	if entityType == EntityTypeOrg {
		col, where = "org_id", "org_id IS NOT NULL"
	}
	var seen []model.BehaviorSeenCategory
	var rollups []model.BehaviorRollup

	type catRow struct {
		EntityID int64
		Category string
		Count    int64
	}
	for kind, src := range map[string]struct {
		sql    string
		metric string
	}{
		"model": {
			sql:    fmt.Sprintf("SELECT %[1]s AS entity_id, model_used AS category, COUNT(*) AS count FROM usage_logs WHERE created_at >= ? AND created_at < ? AND %[2]s GROUP BY %[1]s, model_used", col, where),
			metric: "model",
		},
		"grule": {
			sql:    fmt.Sprintf("SELECT %[1]s AS entity_id, guardrail_rule AS category, COUNT(*) AS count FROM usage_logs WHERE created_at >= ? AND created_at < ? AND %[2]s AND guardrail_rule IS NOT NULL AND guardrail_rule <> '' GROUP BY %[1]s, guardrail_rule", col, where),
			metric: "grule",
		},
	} {
		var rows []catRow
		if err := raw(a.DB, ctx, src.sql, from.UTC(), to.UTC()).Scan(&rows).Error; err != nil {
			return nil, nil, fmt.Errorf("aggregate %s categories %s: %w", entityType, kind, err)
		}
		for _, r := range rows {
			seen = append(seen, model.BehaviorSeenCategory{
				EntityType: entityType, EntityID: r.EntityID, Kind: kind,
				Category: r.Category, FirstSeenAt: hour, LastSeenAt: hour,
			})
			num := float64(r.Count)
			rollups = append(rollups, rl(entityType, r.EntityID, src.metric+":"+r.Category, "hour", hour, &num, nil, r.Count))
		}
	}
	return seen, rollups, nil
}

// histKey 定位一个 B 类直方图（实体×指标）。
type histKey struct {
	et  string
	id  int64
	met string
}

// ReaggregateDayHistograms 从原始行重算 B 类日直方图（D11：比较窗/日桶分布由作业
// 内存直算，不依赖小时行合成）。§5.5 缺失约定：cache_hit 请求与 NULL first_token
// 不入池；latency ≤ 0 由直方图 Add 丢弃。
func (a *Aggregator) ReaggregateDayHistograms(ctx context.Context, dayStart time.Time) error {
	from, to := dayStart.Truncate(24*time.Hour), dayStart.Truncate(24*time.Hour).Add(24*time.Hour)
	if a.now().UTC().Before(to) {
		to = a.now().UTC() // 今天只算到当前时刻
	}
	hists := map[histKey]*LogHistogram{}
	// 列别名对齐 gorm 命名策略（key_id/latency/first_token），否则 ScanRows
	// 按列名匹配字段，latency_ms 之类扫不进 Latency（按 0 丢弃，直方图全空）。
	sql := `
SELECT api_key_id AS key_id, org_id, latency_ms AS latency, first_token_ms AS first_token
FROM usage_logs
WHERE created_at >= ? AND created_at < ? AND NOT cache_hit`
	rows, err := raw(a.DB, ctx, sql, from.UTC(), to.UTC()).Rows()
	if err != nil {
		return fmt.Errorf("stream latency rows %s: %w", from.Format(time.RFC3339), err)
	}
	defer rows.Close()
	type latencyRow struct {
		KeyID      *int64
		OrgID      *int64
		Latency    int64
		FirstToken *int64
	}
	for rows.Next() {
		var r latencyRow
		if err := a.DB.ScanRows(rows, &r); err != nil {
			return fmt.Errorf("scan latency row: %w", err)
		}
		if r.KeyID != nil {
			addHist(hists, histKey{EntityTypeKey, *r.KeyID, MetricLatency}, float64(r.Latency))
			if r.FirstToken != nil {
				addHist(hists, histKey{EntityTypeKey, *r.KeyID, MetricFirstToken}, float64(*r.FirstToken))
			}
		}
		if r.OrgID != nil {
			addHist(hists, histKey{EntityTypeOrg, *r.OrgID, MetricLatency}, float64(r.Latency))
			if r.FirstToken != nil {
				addHist(hists, histKey{EntityTypeOrg, *r.OrgID, MetricFirstToken}, float64(*r.FirstToken))
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate latency rows: %w", err)
	}
	var rollups []model.BehaviorRollup
	for k, h := range hists {
		if h.Count() == 0 {
			continue
		}
		data, err := h.MarshalJSON()
		if err != nil {
			return fmt.Errorf("marshal histogram %v: %w", k, err)
		}
		s := string(data)
		rollups = append(rollups, model.BehaviorRollup{
			EntityType: k.et, EntityID: k.id, Metric: k.met, Granularity: "day",
			BucketTs: from.UTC(), ValueHist: &s, SampleCount: int64(h.Count()),
		})
	}
	return UpsertRollups(ctx, a.DB, rollups)
}

func addHist(hists map[histKey]*LogHistogram, k histKey, x float64) {
	if x <= 0 {
		return
	}
	h, ok := hists[k]
	if !ok {
		h = NewLogHistogram()
		hists[k] = h
	}
	h.Add(x)
}

// RunHourlyCycle 执行一轮小时作业（§5.3 v4 口径）：重聚合最近 48h（**跳过未变
// 小时**——48h 窗口的意图是兜迟到数据，不是无条件重做；稳态下仅最新小时变化，
// 逐小时重做 48 个未变小时是规格基准实测超线的主因，79.9s 中的 ~57s）→ 重算
// 受影响实体快照 → B 类日直方图（今天+昨天）。锁与续租由调用方（Run）持有。
func (a *Aggregator) RunHourlyCycle(ctx context.Context) error {
	now := a.now().UTC()
	first := now.Add(-reaggregateLookback).Truncate(time.Hour)
	affected := map[[2]any]bool{}
	reaggStart := time.Now()
	changed := 0
	for h := first; h.Before(now); h = h.Add(time.Hour) {
		unchanged, err := a.hourUnchanged(ctx, h)
		if err != nil {
			return err
		}
		if unchanged {
			continue // 该小时无新行/无删行（usage 计数 == rollup 记账）
		}
		changed++
		seen, err := a.ReaggregateHour(ctx, h)
		if err != nil {
			return err
		}
		for _, s := range seen {
			affected[[2]any{s.EntityType, s.EntityID}] = true
		}
	}
	reaggDur := time.Since(reaggStart)
	// B 类日桶：今天 + 昨天（幂等整值覆盖）。
	dayStart := time.Now()
	for d := now.Truncate(24*time.Hour).AddDate(0, 0, -1); !d.After(now.Truncate(24 * time.Hour)); d = d.AddDate(0, 0, 1) {
		if err := a.ReaggregateDayHistograms(ctx, d); err != nil {
			return err
		}
	}
	dayDur := time.Since(dayStart)
	// 受影响实体 = 有变化小时里有目录活动的实体（含请求的实体必有 model 目录行）。
	entities := make([]EntityRef, 0, len(affected))
	for k := range affected {
		entities = append(entities, EntityRef{Type: k[0].(string), ID: k[1].(int64)})
	}
	recStart := time.Now()
	if err := a.RecomputeBaselines(ctx, entities); err != nil {
		return err
	}
	// 分阶段计时为永久观测点（规格基准归因 + 生产观测）。
	slog.Info("behavior hourly cycle phases",
		"reagg", reaggDur.Round(time.Millisecond), "changed_hours", changed,
		"dayhist", dayDur.Round(time.Millisecond),
		"recompute", time.Since(recStart).Round(time.Millisecond), "entities", len(entities))
	return nil
}

// hourUnchanged 判断该小时是否无需重聚合：usage_logs 行数 == **key 层** request_rate
// 桶的 sample_count 记账和（必须滤 entity_type='key'——org 层同指标记账会把
// dst 翻倍，导致跳过永不生效、每小时全量重做：规格基准 changed_hours=30 实测
// 揪出的 bug）。迟到/删除使计数分歧 → 重做；两侧皆 0（夜里无流量的空小时）也
// 跳过。已见集语义不受影响：无新行 = 无活动，last_seen 不该前进。
func (a *Aggregator) hourUnchanged(ctx context.Context, h time.Time) (bool, error) {
	var src, dst int64
	if err := raw(a.DB, ctx,
		"SELECT COUNT(*) FROM usage_logs WHERE created_at >= ? AND created_at < ?",
		h, h.Add(time.Hour)).Scan(&src).Error; err != nil {
		return false, fmt.Errorf("count usage hour %s: %w", h, err)
	}
	if err := raw(a.DB, ctx,
		"SELECT COALESCE(SUM(sample_count), 0) FROM behavior_rollups WHERE entity_type = ? AND metric = ? AND granularity = 'hour' AND bucket_ts = ?",
		EntityTypeKey, MetricRequestRate, h).Scan(&dst).Error; err != nil {
		return false, fmt.Errorf("count rollup hour %s: %w", h, err)
	}
	return src == dst, nil
}

// Cleanup 执行保留期清理（小时桶 35 天、日桶与已见集 180 天）——每日全量轮调用。
func (a *Aggregator) Cleanup(ctx context.Context) error {
	now := a.now().UTC()
	if err := exec(a.DB, ctx,
		"DELETE FROM behavior_rollups WHERE (granularity = 'hour' AND bucket_ts < ?) OR (granularity = 'day' AND bucket_ts < ?)",
		now.Add(-35*24*time.Hour), now.Add(-180*24*time.Hour)).Error; err != nil {
		return fmt.Errorf("cleanup rollups: %w", err)
	}
	if err := exec(a.DB, ctx,
		"DELETE FROM behavior_seen_categories WHERE last_seen_at < ?",
		now.Add(-180*24*time.Hour)).Error; err != nil {
		return fmt.Errorf("cleanup seen categories: %w", err)
	}
	return nil
}

// ---- 基线快照重算（D8：每轮从 rollups 全量重算覆盖）----

// EntityRef 标识一个基线主体。
type EntityRef struct {
	Type string
	ID   int64
}

// RecomputeBaselines 重算给定实体的全部指标快照。entities 为空则无操作（每日全量轮
// 由 RunDailyCycle 传入全体实体）。
func (a *Aggregator) RecomputeBaselines(ctx context.Context, entities []EntityRef) error {
	if len(entities) == 0 {
		return nil
	}
	now := a.now().UTC()
	winStart := now.Truncate(time.Hour).Add(-baselineWindowDays * 24 * time.Hour)
	var snapshots []model.BehaviorBaseline
	for _, e := range entities {
		var rollups []model.BehaviorRollup
		if err := a.DB.WithContext(ctx).
			Where("entity_type = ? AND entity_id = ? AND bucket_ts >= ?", e.Type, e.ID, winStart).
			Order("bucket_ts ASC").
			Find(&rollups).Error; err != nil {
			return fmt.Errorf("load rollups %v: %w", e, err)
		}
		snapshots = append(snapshots, computeSnapshots(e, rollups, winStart, now)...)
	}
	return UpsertBaselines(ctx, a.DB, snapshots)
}

// computeSnapshots 从 rollup 行计算快照（纯函数，可测）。
//
// 快照布局（§5.2）：A 类小时指标 → 24 个 hour-of-day 桶（"0".."23"）；cost → 单个
// "DAY" 桶（小时值按 UTC 日求和后的日序列分位，§5.5）；C 类 → 单个 "ALL" 桶
// （池化比率存 p50，p90/p99 置同值，ewma 为小时比率序列的 EWMA，obs_count=池化
// 分母——C 类打分只用 p50 与 obs_count）；B 类 → 单个 "ALL" 池化直方图分位。
func computeSnapshots(e EntityRef, rollups []model.BehaviorRollup, winStart, winEnd time.Time) []model.BehaviorBaseline {
	type series struct {
		vals []float64 // 小时序列（value_num，时间序）
		ts   []time.Time
		den  []float64 // C 类分母（并行）
	}
	perMetric := map[string]*series{}
	hists := map[string]*LogHistogram{}
	for _, r := range rollups {
		switch {
		case r.Granularity == "day" && r.ValueHist != nil:
			h, err := ParseLogHistogram(*r.ValueHist)
			if err == nil {
				if hists[r.Metric] == nil {
					hists[r.Metric] = NewLogHistogram()
				}
				hists[r.Metric].Merge(h)
			}
		case r.Granularity == "hour" && r.ValueNum != nil && !isCategoryMetric(r.Metric):
			s := perMetric[r.Metric]
			if s == nil {
				s = &series{}
				perMetric[r.Metric] = s
			}
			s.vals = append(s.vals, *r.ValueNum)
			s.ts = append(s.ts, r.BucketTs)
			if r.ValueDen != nil {
				s.den = append(s.den, *r.ValueDen)
			}
		}
	}
	var out []model.BehaviorBaseline
	base := func(metric, key string) model.BehaviorBaseline {
		return model.BehaviorBaseline{
			EntityType: e.Type, EntityID: e.ID, Metric: metric, BucketKey: key,
			WindowStart: winStart, WindowEnd: winEnd, ModelVersion: modelVersion,
		}
	}
	for metric, s := range perMetric {
		if isRatioMetric(metric) {
			// C 类：池化比率 + 小时比率的 EWMA。
			var num, den float64
			var ratioSeries []float64
			for i := range s.vals {
				if i < len(s.den) && s.den[i] > 0 {
					num += s.vals[i]
					den += s.den[i]
					ratioSeries = append(ratioSeries, s.vals[i]/s.den[i])
				}
			}
			b := base(metric, "ALL")
			if den > 0 {
				b.P50 = num / den
				b.P90, b.P99 = b.P50, b.P50
				b.EWMA, b.EWMAVar = ewmaStats(ratioSeries)
				b.ObsCount = int(den)
			}
			out = append(out, b)
			continue
		}
		if isCostMetric(metric) {
			// cost 冻结契约（§5.5）：UTC 日序列（24 个小时桶按日求和 → 日值分位），bucket_key=DAY。
			daySums := map[string]float64{}
			for i, v := range s.vals {
				day := s.ts[i].UTC().Format("2006-01-02")
				daySums[day] += v
			}
			sums := make([]float64, 0, len(daySums))
			for _, v := range daySums {
				sums = append(sums, v)
			}
			b := base(metric, "DAY")
			b.P50, b.P90, b.P99 = nearestRank(sums, 0.50), nearestRank(sums, 0.90), nearestRank(sums, 0.99)
			b.EWMA, b.EWMAVar = ewmaStats(s.vals) // EWMA 仍走小时序列（趋势通道）
			b.ObsCount = len(sums)
			out = append(out, b)
			continue
		}
		// A 类：hour-of-day 分桶分位 + 全序列 EWMA。
		byHour := make([][]float64, 24)
		for i, v := range s.vals {
			byHour[s.ts[i].UTC().Hour()] = append(byHour[s.ts[i].UTC().Hour()], v)
		}
		ew, ewVar := ewmaStats(s.vals)
		for h, vals := range byHour {
			if len(vals) == 0 {
				continue
			}
			b := base(metric, fmt.Sprintf("%d", h))
			b.P50, b.P90, b.P99 = nearestRank(vals, 0.50), nearestRank(vals, 0.90), nearestRank(vals, 0.99)
			b.EWMA, b.EWMAVar = ew, ewVar
			b.ObsCount = len(vals)
			out = append(out, b)
		}
	}
	for metric, h := range hists {
		if h.Count() == 0 {
			continue
		}
		b := base(metric, "ALL")
		b.P50 = h.Quantile(0.50)
		b.P90 = h.Quantile(0.90)
		b.P99 = h.Quantile(0.99)
		b.ObsCount = int(h.Count())
		out = append(out, b)
	}
	return out
}

func isCategoryMetric(metric string) bool {
	return len(metric) > 6 && (metric[:6] == "model:" || metric[:6] == "grule:")
}

func isRatioMetric(metric string) bool {
	return metric == MetricErrGw || metric == MetricErrUp || metric == MetricGrTrigger
}

func isCostMetric(metric string) bool {
	return metric == MetricCostCNY || metric == MetricCostUSD
}

// nearestRank 最近秩分位数（经验分布，28 观测下 P90≈第 3 大值——D12）。
func nearestRank(vals []float64, q float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	idx := int(math.Ceil(q * float64(len(sorted))))
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// ewmaAlpha 是 EWMA 步长：7 天半衰期折算到小时序列 α = 1 − 0.5^(1/168)
// （math.Pow 非常量表达式，故为包级 var 而非 const）。
var ewmaAlpha = 1 - math.Pow(0.5, 1.0/168.0)

// ewmaStats 计算序列的 EWMA 均值与方差（方差用标准 EWMA 方差估计
// var ← (1−α)(var + α·dev²)）。空序列返回 (0, 0)。
func ewmaStats(vals []float64) (float64, float64) {
	alpha := ewmaAlpha
	if len(vals) == 0 {
		return 0, 0
	}
	ew := vals[0]
	var variance float64
	for _, v := range vals[1:] {
		dev := v - ew
		ew += alpha * dev
		variance = (1 - alpha) * (variance + alpha*dev*dev)
	}
	return ew, variance
}
