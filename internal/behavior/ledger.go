package behavior

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// 异常台账写入与生命周期（规划冻结 v4.0 §5.9，D16）。
//
// 合并语义：UPDATE 先行、锚定 last_hit_at 的滚动 4h 窗（03:59 与 04:00 的相邻
// 事件正确合并；连续命中可长期存活）；只合并 status='open'——confirmed/dismissed
// 为终态，其后的新命中开新案引用前案（"不重开"由"只合并 open"直接蕴含）。
// UNIQUE(event_fingerprint, merge_bucket) 仅作并发创建兜底：INSERT 撞键 → 回退
// UPDATE 重试一次（并发对手已建案，合并必然成功）。
//
// score_peak 的取大用 CASE 表达式而非 GREATEST——SQLite 无 GREATEST，CASE 三方言
// 同形（M1 upsert 的可移植纪律同款）。

const anomalyMergeWindow = 4 * time.Hour

// anomalyStatusOpen 等状态常量（表内为字符串，集中定义防拼写漂移）。
const (
	AnomalyStatusOpen      = "open"
	AnomalyStatusConfirmed = "confirmed"
	AnomalyStatusDismissed = "dismissed"
	AnomalyStatusExpired   = "expired"

	BandSuspicious = "suspicious"
	BandAnomalous  = "anomalous"
	BandCritical   = "critical"
)

// BandOf 按冻结分档阈值（固定常量，D14：不随容忍度/配置变化）。
func BandOf(score float64) string {
	switch {
	case score >= 95:
		return BandCritical
	case score >= 80:
		return BandAnomalous
	case score >= 60:
		return BandSuspicious
	default:
		return ""
	}
}

// RecordHit 记录一次异常命中：同指纹且距 last_hit_at ≤4h 的 open 事件并入
// （hit_count+1、score_peak 取大、窗口右移），否则新建事件。返回是否并入既有事件。
func RecordHit(ctx context.Context, db *gorm.DB, evt *model.BehaviorAnomaly) (bool, error) {
	t := evt.LastHitAt
	merged, err := tryMergeAnomaly(ctx, db, evt, t)
	if err != nil {
		return false, err
	}
	if merged {
		return true, nil
	}
	if err := db.WithContext(ctx).Create(evt).Error; err != nil {
		// 并发创建兜底：对手已建同 (fingerprint, merge_bucket) 案 → 合并必然成功。
		merged, mErr := tryMergeAnomaly(ctx, db, evt, t)
		if mErr != nil {
			return false, fmt.Errorf("record anomaly: create %v, retry-merge %v", err, mErr)
		}
		if merged {
			return true, nil
		}
		return false, fmt.Errorf("record anomaly: %w", err)
	}
	return false, nil
}

func tryMergeAnomaly(ctx context.Context, db *gorm.DB, evt *model.BehaviorAnomaly, t time.Time) (bool, error) {
	res := db.WithContext(ctx).
		Model(&model.BehaviorAnomaly{}).
		// last_hit_at <= t：同小时幂等重跑（t 与既有 last_hit 相等）也走合并，
		// 否则合并与新建双双落空导致报错。
		Where("event_fingerprint = ? AND status = ? AND last_hit_at > ? AND last_hit_at <= ?",
			evt.EventFingerprint, AnomalyStatusOpen, t.Add(-anomalyMergeWindow), t).
		Updates(map[string]any{
			"hit_count":   gorm.Expr("hit_count + 1"),
			"last_hit_at": t,
			"score":       evt.Score,
			"score_peak":  gorm.Expr("CASE WHEN score_peak < ? THEN ? ELSE score_peak END", evt.Score, evt.Score),
			"window_end":  evt.WindowEnd,
			"dimensions":  nullableStr(evt.Dimensions),
			"updated_at":  time.Now().UTC(),
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// SweepLedger 执行台账清扫（小时作业调用；每日全量轮再兜底一遍）：
//
//  1. 过期：open 且 7 天无新命中 → expired(inactivity)；
//  2. 并发兜底合并：同指纹多个 open 事件按 last_hit_at 排序贪心聚类（相邻间隔
//     ≤4h 即同簇），年长者保留（hit_count 求和、score_peak/last_hit_at 取大），
//     其余删除——跨 4h 桶边界并发插入的最终一致，窗口 ≤1 作业周期。
func (a *Aggregator) SweepLedger(ctx context.Context) error {
	now := a.now().UTC()
	if err := exec(a.DB, ctx,
		"UPDATE behavior_anomalies SET status = ?, closed_reason = 'inactivity', updated_at = ? WHERE status = ? AND last_hit_at < ?",
		AnomalyStatusExpired, now, AnomalyStatusOpen, now.Add(-7*24*time.Hour)).Error; err != nil {
		return fmt.Errorf("expire anomalies: %w", err)
	}

	var open []model.BehaviorAnomaly
	// 只看近 8h 的 open 事件——更早的不可能与新命中构成 4h 链（贪心聚类的安全剪枝）。
	if err := a.DB.WithContext(ctx).
		Where("status = ? AND last_hit_at > ?", AnomalyStatusOpen, now.Add(-2*anomalyMergeWindow)).
		Order("last_hit_at ASC").
		Find(&open).Error; err != nil {
		return fmt.Errorf("load open anomalies for sweep: %w", err)
	}
	byFp := map[string][]model.BehaviorAnomaly{}
	for _, e := range open {
		byFp[e.EventFingerprint] = append(byFp[e.EventFingerprint], e)
	}
	for fp, group := range byFp {
		if len(group) < 2 {
			continue
		}
		// group 已按 last_hit_at 升序；贪心聚类（滚动 4h 锚点）。
		keep := group[0]
		for i := 1; i < len(group); i++ {
			if group[i].LastHitAt.Sub(keep.LastHitAt) <= anomalyMergeWindow {
				if err := a.DB.WithContext(ctx).
					Model(&model.BehaviorAnomaly{}).
					Where("id = ?", keep.ID).
					Updates(map[string]any{
						"hit_count":  keep.HitCount + group[i].HitCount,
						"score_peak": maxF(keep.ScorePeak, group[i].ScorePeak),
						"last_hit_at": func() time.Time {
							if group[i].LastHitAt.After(keep.LastHitAt) {
								return group[i].LastHitAt
							}
							return keep.LastHitAt
						}(),
						"updated_at": now,
					}).Error; err != nil {
					return fmt.Errorf("sweep merge %s: %w", fp, err)
				}
				keep.HitCount += group[i].HitCount
				keep.ScorePeak = maxF(keep.ScorePeak, group[i].ScorePeak)
				if group[i].LastHitAt.After(keep.LastHitAt) {
					keep.LastHitAt = group[i].LastHitAt
				}
				if err := a.DB.WithContext(ctx).Where("id = ?", group[i].ID).Delete(&model.BehaviorAnomaly{}).Error; err != nil {
					return fmt.Errorf("sweep delete %s: %w", fp, err)
				}
			} else {
				keep = group[i] // 新簇
			}
		}
	}
	return nil
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// ---- 反馈学习闭环（规划冻结 v4.0 §5.6，D10/D14）----

// toleranceStepFloor/toleranceCeil 是乘子硬顶（棘轮）：dismissed 放宽 ×1.1、
// confirmed 收紧 ×0.9，重复反馈累计到触界；永不改写 rollups 或基线分位数。
const (
	toleranceFloor = 0.5
	toleranceCeil  = 2.0
)

// FiredDimensions 解析事件的 dimensions 快照，返回命中维度（s'ᵢ≥40）的维度名
// ——反馈只作用于事件命中过的维度（§5.6）。
func FiredDimensions(dimensionsJSON string) []string {
	snaps := firedSnapshots(dimensionsJSON)
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.Metric)
	}
	return out
}

func firedSnapshots(dimensionsJSON string) []dimSnapshot {
	if dimensionsJSON == "" {
		return nil
	}
	var snaps []dimSnapshot
	if err := json.Unmarshal([]byte(dimensionsJSON), &snaps); err != nil {
		return nil
	}
	fired := make([]dimSnapshot, 0, len(snaps))
	for _, s := range snaps {
		if s.ScoreAdj >= 40 {
			fired = append(fired, s)
		}
	}
	return fired
}

// BuildFingerprintCandidate 从已确认事件生成行为指纹候选（§5.6/D10：confirmed
// 额外产出，人在环复核，审批不自动生效）。无命中维度（s'ᵢ≥40）返回 nil。
func BuildFingerprintCandidate(ev *model.BehaviorAnomaly) *model.BehaviorFingerprint {
	dimsJSON := ""
	if ev.Dimensions != nil {
		dimsJSON = *ev.Dimensions
	}
	fired := firedSnapshots(dimsJSON)
	if len(fired) == 0 {
		return nil
	}
	names := make([]string, 0, len(fired))
	for _, s := range fired {
		names = append(names, s.Metric)
	}
	sort.Strings(names)
	tpl, err := json.Marshal(fired)
	if err != nil {
		return nil
	}
	return &model.BehaviorFingerprint{
		EntityType:    ev.EntityType,
		EntityID:      ev.EntityID,
		DimensionSet:  strings.Join(names, ","),
		Template:      string(tpl),
		SourceEventID: &ev.ID,
		HitCount:      1,
		Status:        "candidate",
	}
}

// UpsertFingerprintCandidate 幂等落候选：同 (entity, dimension_set) 已有 candidate
// → hit_count+1、模板与来源事件更新为最新；approved/retired 为终态不动（同模式
// 再确认不重复计数——复核结论优先）。管理端低频操作，读-改-写竞态可接受。
func UpsertFingerprintCandidate(ctx context.Context, db *gorm.DB, cand *model.BehaviorFingerprint) error {
	if cand == nil {
		return nil
	}
	var existing model.BehaviorFingerprint
	err := db.WithContext(ctx).
		Where("entity_type = ? AND entity_id = ? AND dimension_set = ?",
			cand.EntityType, cand.EntityID, cand.DimensionSet).
		First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return db.WithContext(ctx).Create(cand).Error
	case err != nil:
		return err
	case existing.Status != "candidate":
		return nil // 终态：复核结论优先，不覆盖
	default:
		existing.HitCount++
		existing.Template = cand.Template
		existing.SourceEventID = cand.SourceEventID
		existing.UpdatedAt = time.Now().UTC()
		return db.WithContext(ctx).Save(&existing).Error
	}
}

// ApplyToleranceFeedback 对事件命中维度施加反馈并 upsert 容忍度：
// decision="dismiss" → t ← min(2.0, t×1.1)；"confirm" → t ← max(0.5, t×0.9)。
// 无行视为 t=1.0 起步；读取-调整-upsert 的竞态窗口由管理端低频操作兜底（文档 §5.6）。
func ApplyToleranceFeedback(ctx context.Context, db *gorm.DB, entityType string, entityID int64, decision string, dims []string, now time.Time) error {
	if decision != AnomalyStatusDismissed && decision != AnomalyStatusConfirmed {
		return fmt.Errorf("invalid feedback decision %q", decision)
	}
	for _, m := range dims {
		t := 1.0
		var row model.BehaviorTolerance
		if err := db.WithContext(ctx).
			Where("entity_type = ? AND entity_id = ? AND metric = ?", entityType, entityID, m).
			First(&row).Error; err == nil {
			t = row.Multiplier
		}
		if decision == AnomalyStatusDismissed {
			t = math.Min(toleranceCeil, t*1.1)
		} else {
			t = math.Max(toleranceFloor, t*0.9)
		}
		if err := UpsertTolerance(ctx, db, model.BehaviorTolerance{
			EntityType: entityType, EntityID: entityID, Metric: m,
			Multiplier: t, LastFeedbackAt: now.UTC(),
		}); err != nil {
			return fmt.Errorf("apply tolerance feedback %s/%s: %w", entityType, m, err)
		}
	}
	return nil
}
