package behavior

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// 作业循环（规划冻结 v4.0 §5.3）。两种作业各自持锁（单实例语义）：
//
//	hourly：重聚合 48h → 受影响实体快照重算 → B 类日直方图（今天+昨天）
//	daily ：全体实体快照全量重算（窗口滑动一致性）+ 保留期清理 + 已见集 GC
//
// 租约 5 分钟，运行期每 60s 续租；续租失败（被抢占）立即中止本轮（D15）。

const (
	JobHourly    = "behavior_hourly"
	JobDaily     = "behavior_daily"
	jobLease     = 5 * time.Minute
	renewPeriod  = 60 * time.Second
	hourlyPeriod = time.Hour
	dailyPeriod  = 24 * time.Hour
)

// ErrLeaseLost 表示作业运行中租约被抢占（本轮回滚语义由幂等写承担，§5.3 取舍表）。
var ErrLeaseLost = errors.New("behavior job lease lost")

// Run 启动小时/日作业循环（每个立即先跑一轮，之后按周期）。阻塞直到 ctx 取消。
func Run(ctx context.Context, db *gorm.DB) {
	go loop(ctx, db, JobHourly, hourlyPeriod, runHourlyJob)
	go loop(ctx, db, JobDaily, dailyPeriod, runDailyJob)
	<-ctx.Done()
}

func loop(ctx context.Context, db *gorm.DB, job string, period time.Duration, fn func(context.Context, *Aggregator) error) {
	agg := &Aggregator{DB: db}
	// 启动即跑一轮（小部署上让基线尽快开始积累；抢不到锁则等下个周期）。
	runOnce(ctx, db, agg, job, fn)
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runOnce(ctx, db, agg, job, fn)
		}
	}
}

func runOnce(ctx context.Context, db *gorm.DB, agg *Aggregator, job string, fn func(context.Context, *Aggregator) error) {
	owner := NewOwnerToken()
	fence, ok, err := AcquireJobLock(ctx, db, job, owner, jobLease)
	if err != nil {
		slog.Warn("behavior job acquire failed", "job", job, "error", err)
		return
	}
	if !ok {
		return // 他人持有——正常竞争
	}
	start := time.Now()
	err = withLease(ctx, db, job, owner, func(workCtx context.Context) error {
		return fn(workCtx, agg)
	})
	releaseErr := ReleaseJobLock(ctx, db, job, owner)
	switch {
	case errors.Is(err, ErrLeaseLost):
		slog.Warn("behavior job lease lost mid-run (preempted); idempotent writes make this safe", "job", job, "fence", fence)
	case err != nil:
		slog.Error("behavior job failed", "job", job, "fence", fence, "error", err)
	default:
		slog.Info("behavior job done", "job", job, "fence", fence, "elapsed", time.Since(start).Round(time.Millisecond))
	}
	if releaseErr != nil {
		slog.Warn("behavior job release failed", "job", job, "error", releaseErr)
	}
}

// withLease 持锁执行 fn：60s 续租看护，续租失败取消 fn 的 ctx 并返回 ErrLeaseLost。
func withLease(ctx context.Context, db *gorm.DB, job, owner string, fn func(context.Context) error) error {
	renewCtx, renewCancel := context.WithCancel(ctx)
	defer renewCancel()
	workCtx, workCancel := context.WithCancel(ctx)
	defer workCancel()
	lost := make(chan struct{})
	go func() {
		t := time.NewTicker(renewPeriod)
		defer t.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-t.C:
				ok, err := RenewJobLock(renewCtx, db, job, owner, jobLease)
				if err != nil {
					slog.Warn("behavior job renew failed", "job", job, "error", err)
				}
				if err != nil || !ok {
					close(lost)
					workCancel() // 立即中止本轮
					return
				}
			}
		}
	}()
	err := fn(workCtx)
	select {
	case <-lost:
		return ErrLeaseLost
	default:
	}
	return err
}

func runHourlyJob(ctx context.Context, agg *Aggregator) error {
	if err := agg.RunHourlyCycle(ctx); err != nil {
		return err
	}
	// 对上一完整小时打分（该小时数据已重聚合、基线已重算）+ 台账清扫（D16）。
	lastComplete := agg.now().UTC().Truncate(time.Hour).Add(-time.Hour)
	scoreStart := time.Now()
	if err := agg.ScoreHour(ctx, lastComplete); err != nil {
		return err
	}
	sweepStart := time.Now()
	if err := agg.SweepLedger(ctx); err != nil {
		return err
	}
	slog.Info("behavior hourly job phases",
		"score", time.Since(scoreStart).Round(time.Millisecond),
		"sweep", time.Since(sweepStart).Round(time.Millisecond))
	return nil
}

func runDailyJob(ctx context.Context, agg *Aggregator) error {
	entities, err := agg.activeEntities(ctx, baselineWindowDays)
	if err != nil {
		return err
	}
	if err := agg.RecomputeBaselines(ctx, entities); err != nil {
		return err
	}
	if err := agg.Cleanup(ctx); err != nil {
		return err
	}
	return agg.SweepLedger(ctx) // 每日全量轮再兜底一遍（§5.9）
}

// activeEntities 返回近 N 天在 rollups 中有桶的全部实体（每日全量轮的全体集）。
func (a *Aggregator) activeEntities(ctx context.Context, days int) ([]EntityRef, error) {
	since := a.now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var rows []struct {
		EntityType string
		EntityID   int64
	}
	if err := a.DB.WithContext(ctx).
		Table("behavior_rollups").
		Select("DISTINCT entity_type, entity_id").
		Where("bucket_ts >= ? AND metric = ?", since, MetricRequestRate).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]EntityRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, EntityRef{Type: r.EntityType, ID: r.EntityID})
	}
	return out, nil
}
