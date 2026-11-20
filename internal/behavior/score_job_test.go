package behavior

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

// TestScoreHourCreatesAndSuppresses 打分接线集成：10× 速率尖峰 → critical 事件；
// 容忍度乘子 2.0 把维度分减半到 50 → 低于起报线不产生事件（D14）；同小时幂等重跑
// 走合并不重复建案。
func TestScoreHourCreatesAndSuppresses(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)

	seedBaseline := func(keyID int64) {
		db.Create(&model.BehaviorBaseline{
			EntityType: EntityTypeKey, EntityID: keyID, Metric: MetricRequestRate,
			BucketKey: strconv.Itoa(hour.UTC().Hour()),
			P50:       10, P90: 10, P99: 11, EWMA: 10, EWMAVar: 1, ObsCount: 28,
			WindowStart: hour.Add(-28 * 24 * time.Hour), WindowEnd: hour, ModelVersion: modelVersion,
		})
	}
	seedRollup := func(keyID int64, v float64) {
		x := v
		db.Create(&model.BehaviorRollup{
			EntityType: EntityTypeKey, EntityID: keyID, Metric: MetricRequestRate,
			Granularity: "hour", BucketTs: hour, ValueNum: &x, SampleCount: int64(v),
		})
	}
	seedBaseline(1)
	seedBaseline(2)
	seedRollup(1, 100) // 10× 基线 → s=100
	seedRollup(2, 100) // 同样 10×，但容忍度 2.0 → s'=50 → 不起报
	db.Create(&model.BehaviorTolerance{
		EntityType: EntityTypeKey, EntityID: 2, Metric: MetricRequestRate,
		Multiplier: 2, LastFeedbackAt: hour,
	})

	agg := &Aggregator{DB: db}
	if err := agg.ScoreHour(ctx, hour); err != nil {
		t.Fatalf("ScoreHour: %v", err)
	}
	var evs []model.BehaviorAnomaly
	db.Find(&evs)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1 (key1 critical; key2 suppressed by tolerance)", len(evs))
	}
	ev := evs[0]
	if ev.EntityID != 1 || ev.Band != BandCritical || ev.Score != 100 {
		t.Fatalf("event: entity=%d band=%s score=%v, want 1/critical/100", ev.EntityID, ev.Band, ev.Score)
	}
	if ev.EventFingerprint == "" || ev.Dimensions == nil || !strings.Contains(*ev.Dimensions, "request_rate") {
		t.Fatalf("event evidence incomplete: fp=%q dims=%v", ev.EventFingerprint, ev.Dimensions)
	}
	if ev.Status != AnomalyStatusOpen || ev.HitCount != 1 {
		t.Fatalf("fresh event: status=%s hit_count=%d", ev.Status, ev.HitCount)
	}

	// 同小时幂等重跑 → 合并（hit_count=2），不重复建案。
	if err := agg.ScoreHour(ctx, hour); err != nil {
		t.Fatalf("ScoreHour re-run: %v", err)
	}
	db.Find(&evs)
	if len(evs) != 1 || evs[0].HitCount != 2 {
		t.Fatalf("after re-run: events=%d hit_count=%d, want 1/2", len(evs), evs[0].HitCount)
	}
}
