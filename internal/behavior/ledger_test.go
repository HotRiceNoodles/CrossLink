package behavior

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

func mkEvt(fp string, t time.Time, score float64) *model.BehaviorAnomaly {
	return &model.BehaviorAnomaly{
		EventFingerprint: fp, MergeBucket: t.Unix() / 14400,
		EntityType: EntityTypeKey, EntityID: 1,
		Score: score, ScorePeak: score, Band: BandAnomalous,
		BaselineModelVer: modelVersion,
		FirstSeenAt:      t, LastHitAt: t, HitCount: 1,
		WindowStart: t, WindowEnd: t.Add(time.Hour),
		Status: AnomalyStatusOpen,
	}
}

// TestRecordHitRollingMerge 验证 D16 合并语义：滚动 4h 窗锚定 last_hit_at
// （固定桶边界不参与合并判定）、跨 03:59/04:00 桶边界正确合并、4h 外开新案、
// confirmed 终态不被重开（只合并 open 的直接蕴含）。
func TestRecordHitRollingMerge(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const fp = "fp-merge-test"
	t0 := time.Date(2026, 11, 20, 3, 59, 0, 0, time.UTC)

	// 首次命中 → 新建。
	if merged, err := RecordHit(ctx, db, mkEvt(fp, t0, 80)); err != nil || merged {
		t.Fatalf("first hit: merged=%v err=%v", merged, err)
	}
	// +30min（跨 4h 桶边界 04:00，但距 last_hit 30min）→ 合并。
	if merged, err := RecordHit(ctx, db, mkEvt(fp, t0.Add(30*time.Minute), 85)); err != nil || !merged {
		t.Fatalf("cross-bucket hit must merge: merged=%v err=%v", merged, err)
	}
	// +3h59m（滚动窗内，链式存活）→ 合并。
	if merged, err := RecordHit(ctx, db, mkEvt(fp, t0.Add(3*time.Hour+59*time.Minute), 90)); err != nil || !merged {
		t.Fatalf("chained hit must merge: merged=%v err=%v", merged, err)
	}
	var ev model.BehaviorAnomaly
	if err := db.Where("event_fingerprint = ?", fp).First(&ev).Error; err != nil {
		t.Fatalf("load event: %v", err)
	}
	if ev.HitCount != 3 || ev.ScorePeak != 90 || ev.Score != 90 {
		t.Fatalf("merged event: hit_count=%d score_peak=%v score=%v, want 3/90/90", ev.HitCount, ev.ScorePeak, ev.Score)
	}

	// 距 last_hit（07:58）超过 4h 的命中 → 新案（滚动窗锚定 last_hit，非 t0：
	// 此前 +4h30m 的尝试距 last_hit 仅 31min，正确并入——链式存活的直接推论）。
	if merged, err := RecordHit(ctx, db, mkEvt(fp, t0.Add(8*time.Hour+29*time.Minute), 70)); err != nil || merged {
		t.Fatalf("hit beyond 4h of last_hit must create new event: merged=%v err=%v", merged, err)
	}
	var n int64
	db.Model(&model.BehaviorAnomaly{}).Where("event_fingerprint = ?", fp).Count(&n)
	if n != 2 {
		t.Fatalf("events = %d, want 2", n)
	}

	// 确认第一案为终态 → 其后的命中只进 open 案，不重开。
	if err := db.Model(&model.BehaviorAnomaly{}).
		Where("event_fingerprint = ? AND hit_count = ?", fp, 3).
		Updates(map[string]any{"status": AnomalyStatusConfirmed, "decided_by": "admin"}).Error; err != nil {
		t.Fatalf("confirm: %v", err)
	}
	// 12:59（新案 last_hit 12:28 之后 31min；已确认案的 last_hit 07:58 不匹配
	// status='open' 条件）→ 只并入 open 案。
	if merged, err := RecordHit(ctx, db, mkEvt(fp, t0.Add(9*time.Hour), 75)); err != nil || !merged {
		t.Fatalf("post-confirm hit must merge into open case: merged=%v err=%v", merged, err)
	}
	var confirmed, openEv model.BehaviorAnomaly
	db.Where("event_fingerprint = ? AND status = ?", fp, AnomalyStatusConfirmed).First(&confirmed)
	db.Where("event_fingerprint = ? AND status = ?", fp, AnomalyStatusOpen).First(&openEv)
	if confirmed.HitCount != 3 {
		t.Fatalf("confirmed case must stay frozen: hit_count=%d", confirmed.HitCount)
	}
	if openEv.HitCount != 2 {
		t.Fatalf("open case hit_count=%d, want 2 (reopen never happens)", openEv.HitCount)
	}
}

// TestSweepLedgerMergesDuplicates 验证并发兜底清扫：跨桶边界并发插入产生的
// 同指纹双 open 事件被合并为年长者（hit_count 求和、score_peak/last_hit 取大）。
func TestSweepLedgerMergesDuplicates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	agg := &Aggregator{DB: db, Now: func() time.Time { return time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC) }}
	const fp = "fp-sweep-test"
	t0 := time.Date(2026, 11, 20, 3, 59, 0, 0, time.UTC)
	// 直接双插模拟并发竞态（绕过 RecordHit 的合并）。
	a := mkEvt(fp, t0, 80)
	b := mkEvt(fp, t0.Add(time.Minute), 95) // 同指纹 04:00——merge_bucket 不同
	if err := db.Create(a).Error; err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("seed b: %v", err)
	}
	if err := agg.SweepLedger(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var rows []model.BehaviorAnomaly
	db.Where("event_fingerprint = ?", fp).Find(&rows)
	if len(rows) != 1 {
		t.Fatalf("rows after sweep = %d, want 1", len(rows))
	}
	if rows[0].HitCount != 2 || rows[0].ScorePeak != 95 {
		t.Fatalf("merged: hit_count=%d score_peak=%v, want 2/95", rows[0].HitCount, rows[0].ScorePeak)
	}
}

// TestSweepLedgerExpiresStale 验证 7 天无命中自动过期（inactivity）。
func TestSweepLedgerExpiresStale(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)
	agg := &Aggregator{DB: db, Now: func() time.Time { return now }}
	stale := mkEvt("fp-stale", now.Add(-8*24*time.Hour), 80)
	if err := db.Create(stale).Error; err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := agg.SweepLedger(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var ev model.BehaviorAnomaly
	db.Where("event_fingerprint = ?", "fp-stale").First(&ev)
	if ev.Status != AnomalyStatusExpired || ev.ClosedReason == nil || *ev.ClosedReason != "inactivity" {
		t.Fatalf("stale event: status=%s closed_reason=%v", ev.Status, ev.ClosedReason)
	}
}
