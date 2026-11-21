package behavior

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"time"

	"github.com/crosslink/internal/model"
)

// 打分接线（规划冻结 v4.0 §5.4/附录 A + §5.9 台账）。ScoreHour 对上一完整小时有
// 数据的实体执行 A/B/C/N 四类打分并写台账——全部离线（D6），只读 rollups/baselines/
// seen 集与 usage_logs 的窄窗口。
//
// 权重为冻结 v1 草案（附录 A；调整即 modelVersion+1）：
//
//	request_rate 0.30 / tokens 0.15 / cost 0.15 / latency 0.10 /
//	error 0.10 / guardrail 0.10 / novelty 0.05 / dormant 0.05
//
// 合成语义：S 只对**参与评分的维度**求加权平均（no-score 维度不参与，冻结文档
// Σw'ᵢ 口径）——单维度实体该维度满分即 S=100。

const scoreThreshold = 60.0 // suspicious 起报线（BandOf 的下界）

// dimSnapshot 是事件 dimensions JSON 里的单维度证据（内嵌基线快照，事后可解释）。
type dimSnapshot struct {
	Metric   string  `json:"metric"`
	Class    string  `json:"class"`
	Score    float64 `json:"s"`
	ScoreAdj float64 `json:"s_prime"` // s/tᵢⱼ（D14）
	Obs      int     `json:"obs"`
	P50      float64 `json:"p50,omitempty"`
	P90      float64 `json:"p90,omitempty"`
	XNow     float64 `json:"x_now,omitempty"`
	Note     string  `json:"note,omitempty"`
}

type hourRollupRow struct {
	EntityType  string
	EntityID    int64
	Metric      string
	ValueNum    float64
	ValueDen    float64
	SampleCount int64
}

// ScoreHour 对 hourStart 所在小时打分并写台账。
func (a *Aggregator) ScoreHour(ctx context.Context, hourStart time.Time) error {
	scoreStart := time.Now()
	hour := hourStart.Truncate(time.Hour)
	dayStart := hour.Truncate(24 * time.Hour)

	// 1) 该小时的 rollup 行（核心指标 + 目录计数）。
	var rows []hourRollupRow
	if err := raw(a.DB, ctx, `
SELECT entity_type, entity_id, metric,
       COALESCE(value_num, 0) AS value_num, COALESCE(value_den, 0) AS value_den, sample_count
FROM behavior_rollups
WHERE granularity = 'hour' AND bucket_ts = ?`, hour).Scan(&rows).Error; err != nil {
		return fmt.Errorf("score: load hour rollups: %w", err)
	}
	core := map[EntityRef]map[string]hourRollupRow{}
	catCounts := map[EntityRef]map[string]float64{} // 目录计数（model:*/grule:* → 数量）
	var entities []EntityRef
	seenEntity := map[EntityRef]bool{}
	for _, r := range rows {
		e := EntityRef{Type: r.EntityType, ID: r.EntityID}
		if isCategoryMetric(r.Metric) {
			if catCounts[e] == nil {
				catCounts[e] = map[string]float64{}
			}
			catCounts[e][r.Metric] = r.ValueNum
			continue
		}
		if core[e] == nil {
			core[e] = map[string]hourRollupRow{}
		}
		core[e][r.Metric] = r
		if !seenEntity[e] {
			seenEntity[e] = true
			entities = append(entities, e)
		}
	}
	if len(entities) == 0 {
		return nil
	}

	// 2) key→org 映射（前移：基线继承与 P 通道都需要；事件冗余 org_id 供关联展示）。
	orgOf := a.keyOrgMap(ctx, hour)
	// 3) 基线快照（分块 IN 装配）。加载集 = 活动实体 ∪ 其 org（key 冷启动时
	// 继承 org 基线打分，§5.1；org 自身通常已在活动集中）。
	loadEntities := append([]EntityRef(nil), entities...)
	for _, e := range entities {
		if e.Type == EntityTypeKey {
			if org, ok := orgOf[e.ID]; ok {
				loadEntities = append(loadEntities, EntityRef{Type: EntityTypeOrg, ID: org})
			}
		}
	}
	baselines, err := a.loadBaselines(ctx, loadEntities)
	if err != nil {
		return err
	}
	// 4) 容忍度乘子（默认 1.0）。
	tolerances, err := a.loadTolerances(ctx, entities)
	if err != nil {
		return err
	}
	// 5) B 类比较窗：该小时原始延迟的 log₂ 中位数（流式，离线）。
	yNow := a.hourLatencyMedians(ctx, hour)
	// 6) B 类基线分位（合并 28 天日直方图——快照表无 p25/p75，分布信息只在 rollups）。
	bDist, err := a.latencyBaselineDist(ctx, entities)
	if err != nil {
		return err
	}
	// 7) cost 当日累计（对 DAY 基线的"提前花费"信号）。
	costDay, err := a.costDaySums(ctx, dayStart)
	if err != nil {
		return err
	}
	// 8) 休眠分母：28 天平均小时请求量。
	meanHourly, err := a.meanHourlyRequests(ctx)
	if err != nil {
		return err
	}
	// 9) N 类：本小时首次出现的类别 + org 先验抑制。
	novelty, err := a.noveltyScores(ctx, hour, core, catCounts)
	if err != nil {
		return err
	}
	// 10) P 通道（D17 群体对比——纯内存，零额外查询）。
	peers := computePeerChannel(rows, orgOf)

	for _, e := range entities {
		var orgBmap map[string]map[string]model.BehaviorBaseline
		if e.Type == EntityTypeKey {
			if org, ok := orgOf[e.ID]; ok {
				orgBmap = baselines[EntityRef{Type: EntityTypeOrg, ID: org}]
			}
		}
		evt := a.scoreEntity(e, hour, core[e], baselines[e], orgBmap, tolerances[e], yNow[e], bDist[e], costDay, meanHourly[e], novelty[e], orgOf, peers[e.ID])
		if evt == nil {
			continue
		}
		if _, err := RecordHit(ctx, a.DB, evt); err != nil {
			return fmt.Errorf("score: record anomaly %v: %w", e, err)
		}
	}
	slog.Info("behavior score hour",
		"hour", hour.Format(time.RFC3339), "entities", len(entities),
		"elapsed", time.Since(scoreStart).Round(time.Millisecond))
	return nil
}

// scoreEntity 组装单实体的维度得分并合成（S ≥ 60 才产生事件）。
func (a *Aggregator) scoreEntity(
	e EntityRef, hour time.Time,
	core map[string]hourRollupRow,
	bmap map[string]map[string]model.BehaviorBaseline, // metric → bucketKey → row
	orgBmap map[string]map[string]model.BehaviorBaseline, // key 实体的 org 基线（冷启动继承）
	tol map[string]float64,
	yNow map[string]float64, // latency/first_token → log₂ 中位数
	bdist map[string][3]float64, // latency/first_token → {p25, p50, p75}
	costDay map[EntityRef]map[string]float64,
	meanHourly float64,
	novelty *noveltyHit,
	orgOf map[int64]int64,
	peer *peerStats,
) *model.BehaviorAnomaly {
	var dims []WeightedDimension
	var snaps []dimSnapshot
	bucketKey := fmt.Sprintf("%d", hour.UTC().Hour())

	adj := func(name string, s float64) float64 {
		t := tol[name]
		if t <= 0 {
			t = 1
		}
		return clamp(s/t, 0, 100)
	}
	addDim := func(name, class string, s float64, w float64, obs int, p50, p90, xNow float64, half bool, note string) {
		if s <= 0 {
			return // 无信号维度不参与合成（Compose 分母只含 s'>0，见其注释）
		}
		sAdj := adj(name, s)
		dims = append(dims, WeightedDimension{Metric: name, Score: sAdj, Weight: w, HalfSample: half})
		snaps = append(snaps, dimSnapshot{Metric: name, Class: class, Score: s, ScoreAdj: sAdj, Obs: obs, P50: p50, P90: p90, XNow: xNow, Note: note})
	}
	scoreAMetric := func(x float64, bl model.BehaviorBaseline, ok bool) float64 {
		if !ok {
			return 0
		}
		return ScoreA(x, bl.P50, bl.P90, bl.EWMA, bl.EWMAVar, bl.ObsCount >= MinObsPerBucket, bl.EWMA > 0)
	}

	// request_rate（A）。三分支（§5.1 冷启动分级）：
	//  ① 自身该小时桶基线就绪 → 正常打分；
	//  ② 未立足（自身没有任何一个达到门槛的 request_rate 桶——聚合会让新 key
	//     立刻拥有 obs=1 的不足桶，"无任何基线"永假）→ 继承 org 桶打分（标注
	//     inherited；org 也无 → no-score，G0 门槛 3：新 key 前 14 天零误报）；
	//  ③ 已立足但该小时桶无（休眠时段）→ dormant 维度。
	if rr, ok := core[MetricRequestRate]; ok {
		bl, blOK := bmap[MetricRequestRate][bucketKey]
		established := false
		for _, b := range bmap[MetricRequestRate] {
			if b.ObsCount >= MinObsPerBucket {
				established = true
				break
			}
		}
		switch {
		case blOK && bl.ObsCount >= MinObsPerBucket:
			s := scoreAMetric(rr.ValueNum, bl, true)
			addDim(MetricRequestRate, "A", s, 0.30, bl.ObsCount, bl.P50, bl.P90, rr.ValueNum, bl.ObsCount < 2*MinObsPerBucket, "")
		case !established:
			// 冷启动：继承 org（不继承则 no-score——绝不让新 key 的首个小时
			// 落进 dormant 分支，那会把冷启动当异常）。
			if e.Type == EntityTypeKey {
				if ob, obOK := orgBmap[MetricRequestRate][bucketKey]; obOK && ob.ObsCount >= MinObsPerBucket {
					s := scoreAMetric(rr.ValueNum, ob, true)
					addDim(MetricRequestRate, "A", s, 0.30, ob.ObsCount, ob.P50, ob.P90, rr.ValueNum, true, "inherited:org-baseline")
				}
			}
		case rr.ValueNum > 0:
			share := 1.0
			if meanHourly > 0 {
				share = rr.ValueNum / meanHourly
			}
			addDim("dormant", "N", ScoreN(share, false), 0.05, 0, 0, 0, rr.ValueNum, true, "hour bucket has no baseline activity")
		}
	}

	// tokens（A，三指标取最严）。
	bestTokens, tokObs, tokP50, tokP90, tokX := 0.0, 0, 0.0, 0.0, 0.0
	for _, m := range []string{MetricTokensIn, MetricTokensOut, MetricTokensRea} {
		r, ok := core[m]
		if !ok {
			continue
		}
		bl, blOK := bmap[m][bucketKey]
		if !blOK || bl.ObsCount < MinObsPerBucket {
			continue
		}
		s := scoreAMetric(r.ValueNum, bl, true)
		if s > bestTokens {
			bestTokens, tokObs, tokP50, tokP90, tokX = s, bl.ObsCount, bl.P50, bl.P90, r.ValueNum
		}
	}
	if bestTokens > 0 {
		addDim("tokens", "A", bestTokens, 0.15, tokObs, tokP50, tokP90, tokX, tokObs < 2*MinObsPerBucket, "max of in/out/reasoning")
	}

	// cost（A，对 DAY 基线的当日累计）。
	bestCost, costObs, costP50, costP90, costX := 0.0, 0, 0.0, 0.0, 0.0
	for _, m := range []string{MetricCostCNY, MetricCostUSD} {
		x, ok := costDay[e][m]
		if !ok || x <= 0 {
			continue
		}
		bl, blOK := bmap[m]["DAY"]
		if !blOK || bl.ObsCount < 14 {
			continue
		}
		s := ScoreA(x, bl.P50, bl.P90, bl.EWMA, bl.EWMAVar, true, bl.EWMA > 0)
		if s > bestCost {
			bestCost, costObs, costP50, costP90, costX = s, bl.ObsCount, bl.P50, bl.P90, x
		}
	}
	if bestCost > 0 {
		addDim("cost", "A", bestCost, 0.15, costObs, costP50, costP90, costX, costObs < 28, "running day sum vs DAY baseline")
	}

	// latency（B，含 first_token 取严；快照表无 p25/p75，分位来自合并直方图）。
	bestLat := 0.0
	latNote := ""
	for _, m := range []string{MetricLatency, MetricFirstToken} {
		y, ok := yNow[m]
		q, okQ := bdist[m]
		if !ok || !okQ || q[1] <= 0 {
			continue
		}
		madLog2 := (math.Log2(q[2]) - math.Log2(q[0])) / 2
		s := ScoreB(y, math.Log2(q[1]), madLog2)
		if s > bestLat {
			bestLat, latNote = s, m
		}
	}
	if bestLat > 0 {
		addDim("latency", "B", bestLat, 0.10, 0, 0, 0, math.Pow(2, yNow[latNote]), false, latNote)
	}

	// error（C：gw/up 取严，最小分子 2）。
	bestErr, errObs, errP50, errX, errN := 0.0, 0, 0.0, 0.0, 0.0
	for _, m := range []string{MetricErrGw, MetricErrUp} {
		r, ok := core[m]
		if !ok {
			continue
		}
		bl, blOK := bmap[m]["ALL"]
		if !blOK || bl.ObsCount < 500 {
			continue
		}
		s, elig := ScoreC(bl.P50*float64(bl.ObsCount), float64(bl.ObsCount), r.ValueNum, r.ValueDen, 2)
		if elig && s > bestErr {
			bestErr, errObs, errP50, errX, errN = s, bl.ObsCount, bl.P50, r.ValueNum, r.ValueDen
		}
	}
	if bestErr > 0 {
		addDim("error", "C", bestErr, 0.10, errObs, errP50, 0, errX, errObs < 1000, fmt.Sprintf("num/den=%v/%v", errX, errN))
	}

	// guardrail（C，最小分子 1；分母仅走完全程的请求，§2.2）。
	if r, ok := core[MetricGrTrigger]; ok {
		if bl, blOK := bmap[MetricGrTrigger]["ALL"]; blOK && bl.ObsCount >= 500 {
			s, elig := ScoreC(bl.P50*float64(bl.ObsCount), float64(bl.ObsCount), r.ValueNum, r.ValueDen, 1)
			if elig && s > 0 {
				addDim("guardrail", "C", s, 0.10, bl.ObsCount, bl.P50, 0, r.ValueNum, bl.ObsCount < 1000, fmt.Sprintf("num/den=%v/%v", r.ValueNum, r.ValueDen))
			}
		}
	}

	// novelty（N：本小时新类别，org 先验抑制减半）。
	if novelty != nil && novelty.share > 0 {
		note := fmt.Sprintf("%s:%s", novelty.kind, novelty.category)
		if novelty.suppressed {
			note += " (org prior)"
		}
		addDim("novelty", "N", ScoreN(novelty.share, novelty.suppressed), 0.05, 0, 0, 0, novelty.share, false, note)
	}

	// P 通道（D17 群体对比，M4 补注：可独立开案的维度——grooming 剧本下自身
	// 基线已适应、自身维度全灭，第二道闸必须能独自开案）。dominant_key 无 P 维。
	if e.Type == EntityTypeKey {
		if s, ok := ScorePeer(peer); ok {
			addDim("peer", "P", s, 0.10, peer.nKeys,
				math.Pow(2, peer.median), math.Pow(2, peer.median+peer.mad),
				core[MetricRequestRate].ValueNum, false,
				fmt.Sprintf("ratio=%.1fx z=%.1f org_keys=%d", peer.ratio, peer.z, peer.nKeys))
		}
	}

	if len(dims) == 0 {
		return nil
	}
	S := Compose(dims)
	band := BandOf(S)
	if band == "" {
		return nil // < suspicious：不起报（防台账洪泛）
	}

	// 事件指纹：命中维度集合（s'≥40）的稳定排序（D16/§5.9——集合语义，非 top-3）。
	var hitDims []string
	for _, d := range dims {
		if d.Score >= 40 {
			hitDims = append(hitDims, d.Metric)
		}
	}
	sort.Strings(hitDims)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", e.Type, e.ID, joinDims(hitDims))))
	fp := hex.EncodeToString(sum[:])[:32]

	dimsJSON, _ := json.Marshal(snaps)
	dimsStr := string(dimsJSON)
	var orgID *int64
	if e.Type == EntityTypeKey {
		if org, ok := orgOf[e.ID]; ok {
			orgID = &org
		}
	} else {
		orgID = &e.ID
	}
	return &model.BehaviorAnomaly{
		EventFingerprint: fp,
		MergeBucket:      hour.Unix() / 14400,
		EntityType:       e.Type, EntityID: e.ID, OrgID: orgID,
		Score: S, ScorePeak: S, Band: band,
		Dimensions: &dimsStr, BaselineModelVer: modelVersion,
		FirstSeenAt: hour, LastHitAt: hour.Add(time.Hour),
		HitCount: 1, WindowStart: hour, WindowEnd: hour.Add(time.Hour),
		Status: AnomalyStatusOpen,
	}
}

func joinDims(ds []string) string {
	out := ""
	for i, d := range ds {
		if i > 0 {
			out += ","
		}
		out += d
	}
	return out
}

// ---- 批量装配查询 ----

const loadChunk = 500

func (a *Aggregator) loadBaselines(ctx context.Context, entities []EntityRef) (map[EntityRef]map[string]map[string]model.BehaviorBaseline, error) {
	out := map[EntityRef]map[string]map[string]model.BehaviorBaseline{}
	for _, et := range []string{EntityTypeKey, EntityTypeOrg} {
		ids := make([]int64, 0, len(entities))
		for _, e := range entities {
			if e.Type == et {
				ids = append(ids, e.ID)
			}
		}
		for start := 0; start < len(ids); start += loadChunk {
			end := min(start+loadChunk, len(ids))
			var rows []model.BehaviorBaseline
			if err := a.DB.WithContext(ctx).
				Where("entity_type = ? AND entity_id IN ?", et, ids[start:end]).
				Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("score: load baselines: %w", err)
			}
			for _, r := range rows {
				e := EntityRef{Type: r.EntityType, ID: r.EntityID}
				if out[e] == nil {
					out[e] = map[string]map[string]model.BehaviorBaseline{}
				}
				if out[e][r.Metric] == nil {
					out[e][r.Metric] = map[string]model.BehaviorBaseline{}
				}
				out[e][r.Metric][r.BucketKey] = r
			}
		}
	}
	return out, nil
}

func (a *Aggregator) loadTolerances(ctx context.Context, entities []EntityRef) (map[EntityRef]map[string]float64, error) {
	out := map[EntityRef]map[string]float64{}
	for _, et := range []string{EntityTypeKey, EntityTypeOrg} {
		ids := make([]int64, 0, len(entities))
		for _, e := range entities {
			if e.Type == et {
				ids = append(ids, e.ID)
			}
		}
		for start := 0; start < len(ids); start += loadChunk {
			end := min(start+loadChunk, len(ids))
			var rows []model.BehaviorTolerance
			if err := a.DB.WithContext(ctx).
				Where("entity_type = ? AND entity_id IN ?", et, ids[start:end]).
				Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("score: load tolerances: %w", err)
			}
			for _, r := range rows {
				e := EntityRef{Type: r.EntityType, ID: r.EntityID}
				if out[e] == nil {
					out[e] = map[string]float64{}
				}
				out[e][r.Metric] = r.Multiplier
			}
		}
	}
	return out, nil
}

// hourLatencyMedians 流式计算该小时各实体的 log₂ 延迟中位数（B 类比较窗，
// D11：比较窗分布由作业内存直算，不落库）。database/sql 直连流式（gorm
// Rows+ScanRows 千万行表怪癖同 ReaggregateDayHistograms 注释）。
func (a *Aggregator) hourLatencyMedians(ctx context.Context, hour time.Time) map[EntityRef]map[string]float64 {
	lat := map[EntityRef][]float64{}
	ft := map[EntityRef][]float64{}
	med := func(xs []float64) float64 {
		if len(xs) == 0 {
			return 0
		}
		ls := make([]float64, len(xs))
		for i, x := range xs {
			ls[i] = math.Log2(x)
		}
		sort.Float64s(ls)
		if len(ls)%2 == 1 {
			return ls[len(ls)/2]
		}
		return (ls[len(ls)/2-1] + ls[len(ls)/2]) / 2
	}
	_ = a.streamQuery(ctx, `
SELECT api_key_id, org_id, latency_ms, first_token_ms
FROM usage_logs
WHERE created_at >= ? AND created_at < ? AND NOT cache_hit`,
		[]any{hour, hour.Add(time.Hour)}, func(rows *sql.Rows) error {
			var keyID, orgID, latency, firstToken sql.NullInt64
			if err := rows.Scan(&keyID, &orgID, &latency, &firstToken); err != nil {
				return nil // 单行扫描失败跳过（噪声方向安全）
			}
			if latency.Valid && latency.Int64 > 0 {
				if keyID.Valid {
					lat[EntityRef{EntityTypeKey, keyID.Int64}] = append(lat[EntityRef{EntityTypeKey, keyID.Int64}], float64(latency.Int64))
				}
				if orgID.Valid {
					lat[EntityRef{EntityTypeOrg, orgID.Int64}] = append(lat[EntityRef{EntityTypeOrg, orgID.Int64}], float64(latency.Int64))
				}
			}
			if firstToken.Valid && firstToken.Int64 > 0 {
				if keyID.Valid {
					ft[EntityRef{EntityTypeKey, keyID.Int64}] = append(ft[EntityRef{EntityTypeKey, keyID.Int64}], float64(firstToken.Int64))
				}
				if orgID.Valid {
					ft[EntityRef{EntityTypeOrg, orgID.Int64}] = append(ft[EntityRef{EntityTypeOrg, orgID.Int64}], float64(firstToken.Int64))
				}
			}
			return nil
		})
	out := map[EntityRef]map[string]float64{}
	for e, xs := range lat {
		if m := med(xs); m > 0 {
			if out[e] == nil {
				out[e] = map[string]float64{}
			}
			out[e][MetricLatency] = m
		}
	}
	for e, xs := range ft {
		if m := med(xs); m > 0 {
			if out[e] == nil {
				out[e] = map[string]float64{}
			}
			out[e][MetricFirstToken] = m
		}
	}
	return out
}

// latencyBaselineDist 合并各实体 28 天 B 类日直方图，输出 {p25, p50, p75}
// （快照表只存 p50/p90/p99，MAD 需要的 p25/p75 只在 rollups 直方图里）。
func (a *Aggregator) latencyBaselineDist(ctx context.Context, entities []EntityRef) (map[EntityRef]map[string][3]float64, error) {
	out := map[EntityRef]map[string][3]float64{}
	since := a.now().UTC().Add(-baselineWindowDays * 24 * time.Hour)
	for _, et := range []string{EntityTypeKey, EntityTypeOrg} {
		ids := make([]int64, 0, len(entities))
		for _, e := range entities {
			if e.Type == et {
				ids = append(ids, e.ID)
			}
		}
		for start := 0; start < len(ids); start += loadChunk {
			end := min(start+loadChunk, len(ids))
			var rows []model.BehaviorRollup
			if err := a.DB.WithContext(ctx).
				Where("entity_type = ? AND entity_id IN ? AND granularity = 'day' AND bucket_ts >= ?", et, ids[start:end], since).
				Find(&rows).Error; err != nil {
				return nil, fmt.Errorf("score: load latency histograms: %w", err)
			}
			merged := map[EntityRef]map[string]*LogHistogram{}
			for _, r := range rows {
				if r.ValueHist == nil {
					continue
				}
				e := EntityRef{Type: r.EntityType, ID: r.EntityID}
				h, err := ParseLogHistogram(*r.ValueHist)
				if err != nil {
					continue
				}
				if merged[e] == nil {
					merged[e] = map[string]*LogHistogram{}
				}
				if merged[e][r.Metric] == nil {
					merged[e][r.Metric] = NewLogHistogram()
				}
				merged[e][r.Metric].Merge(h)
			}
			for e, mm := range merged {
				for m, h := range mm {
					if h.Count() == 0 {
						continue
					}
					if out[e] == nil {
						out[e] = map[string][3]float64{}
					}
					out[e][m] = [3]float64{h.Quantile(0.25), h.Quantile(0.50), h.Quantile(0.75)}
				}
			}
		}
	}
	return out, nil
}

// costDaySums 汇总当日小时桶的 cost 累计（"提前花费"信号的比较值）。
func (a *Aggregator) costDaySums(ctx context.Context, dayStart time.Time) (map[EntityRef]map[string]float64, error) {
	var rows []hourRollupRow
	if err := raw(a.DB, ctx, `
SELECT entity_type, entity_id, metric, COALESCE(SUM(value_num), 0) AS value_num, 0 AS value_den, 0 AS sample_count
FROM behavior_rollups
WHERE granularity = 'hour' AND bucket_ts >= ? AND metric IN (?, ?)
GROUP BY entity_type, entity_id, metric`, dayStart, MetricCostCNY, MetricCostUSD).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("score: cost day sums: %w", err)
	}
	out := map[EntityRef]map[string]float64{}
	for _, r := range rows {
		e := EntityRef{Type: r.EntityType, ID: r.EntityID}
		if out[e] == nil {
			out[e] = map[string]float64{}
		}
		out[e][r.Metric] = r.ValueNum
	}
	return out, nil
}

// meanHourlyRequests 计算 28 天平均小时请求量（休眠激活幅值的分母，§5.10）。
func (a *Aggregator) meanHourlyRequests(ctx context.Context) (map[EntityRef]float64, error) {
	var rows []hourRollupRow
	if err := raw(a.DB, ctx, `
SELECT entity_type, entity_id, '' AS metric, AVG(value_num) AS value_num, 0 AS value_den, 0 AS sample_count
FROM behavior_rollups
WHERE granularity = 'hour' AND metric = ? AND bucket_ts >= ?
GROUP BY entity_type, entity_id`, MetricRequestRate, a.now().UTC().Add(-baselineWindowDays*24*time.Hour)).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("score: mean hourly requests: %w", err)
	}
	out := map[EntityRef]float64{}
	for _, r := range rows {
		out[EntityRef{Type: r.EntityType, ID: r.EntityID}] = r.ValueNum
	}
	return out, nil
}

// keyOrgMap 从该小时用量行取 key→org 映射（事件冗余 org_id，§5.6）。
func (a *Aggregator) keyOrgMap(ctx context.Context, hour time.Time) map[int64]int64 {
	var rows []struct {
		KeyID int64
		OrgID int64
	}
	if err := raw(a.DB, ctx, `
SELECT DISTINCT api_key_id AS key_id, org_id
FROM usage_logs
WHERE created_at >= ? AND created_at < ? AND api_key_id IS NOT NULL AND org_id IS NOT NULL`, hour, hour.Add(time.Hour)).Scan(&rows).Error; err != nil {
		return map[int64]int64{}
	}
	out := map[int64]int64{}
	for _, r := range rows {
		out[r.KeyID] = r.OrgID
	}
	return out
}

// noveltyHit 是本小时的新类别命中（§5.10：first_seen_at 落在本小时的已见集行）。
type noveltyHit struct {
	kind       string
	category   string
	share      float64
	suppressed bool // org 先验存在 → 减半
}

func (a *Aggregator) noveltyScores(ctx context.Context, hour time.Time, core map[EntityRef]map[string]hourRollupRow, catCounts map[EntityRef]map[string]float64) (map[EntityRef]*noveltyHit, error) {
	var rows []model.BehaviorSeenCategory
	if err := a.DB.WithContext(ctx).
		Where("first_seen_at >= ? AND first_seen_at < ?", hour, hour.Add(time.Hour)).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("score: load new categories: %w", err)
	}
	out := map[EntityRef]*noveltyHit{}
	for _, r := range rows {
		e := EntityRef{Type: r.EntityType, ID: r.EntityID}
		reqs := core[e][MetricRequestRate].ValueNum
		if reqs <= 0 {
			continue
		}
		// 类别计数来自目录桶（core 不含目录指标）。
		share := catCounts[e][r.Kind+":"+r.Category] / reqs
		if share <= 0 {
			continue
		}
		suppressed := false
		if e.Type == EntityTypeKey {
			// org 先验：同租户其他 key 已知该类别（首次早于本小时）→ 减半。
			suppressed = a.orgSeenBefore(ctx, e.ID, r.Kind, r.Category, hour)
		}
		h := out[e]
		score := ScoreN(share, suppressed)
		if h == nil || score > ScoreN(h.share, h.suppressed) {
			out[e] = &noveltyHit{kind: r.Kind, category: r.Category, share: share, suppressed: suppressed}
		}
	}
	return out, nil
}

func (a *Aggregator) orgSeenBefore(ctx context.Context, keyID int64, kind, category string, hour time.Time) bool {
	var orgID int64
	if err := raw(a.DB, ctx, `
SELECT org_id FROM usage_logs
WHERE api_key_id = ? AND org_id IS NOT NULL AND created_at >= ? AND created_at < ?
LIMIT 1`, keyID, hour.Add(-24*time.Hour), hour.Add(time.Hour)).Scan(&orgID).Error; err != nil || orgID == 0 {
		return false
	}
	var n int64
	// 必须挂 Model——裸 Session 的 Count 不知道表名（报错且被忽略 → 恒 0，
	// suppression 永假：冷启动新 key 的已知模型被当新类别误报的根因）。
	if err := a.DB.WithContext(ctx).
		Model(&model.BehaviorSeenCategory{}).
		Where("entity_type = ? AND entity_id = ? AND kind = ? AND category = ? AND first_seen_at < ?",
			EntityTypeOrg, orgID, kind, category, hour).
		Count(&n).Error; err != nil {
		return false
	}
	return n > 0
}
