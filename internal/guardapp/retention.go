package guardapp

import (
	"context"
	"log/slog"
	"time"

	"github.com/crosslink/internal/repository"
)

// 留存清理（设计 DD-5：guard_events/guard_traces 到期删除）。retention_days
// <=0 显式关闭清理（外部归档等场景）。now 可注入——单测不依赖真实时钟。
//
// 单条 DELETE 语句全量删除（M1 体量下够用；行数增长后如需分批，只改
// RunOnce，接口不动）。traces 先于 events 删除顺序无依赖——两表无外键，
// 事件独立于 trace 留存（trace 只是配对元数据）。
type RetentionJob struct {
	events   *repository.GuardEventRepo
	traces   *repository.GuardTraceRepo
	days     int
	now      func() time.Time
	interval time.Duration
}

func NewRetentionJob(events *repository.GuardEventRepo, traces *repository.GuardTraceRepo, days int) *RetentionJob {
	return &RetentionJob{
		events:   events,
		traces:   traces,
		days:     days,
		now:      time.Now,
		interval: 24 * time.Hour, // 每日一扫；启动即跑一次补上停机窗口
	}
}

// RunOnce 执行一轮清理，返回 (events 删除数, traces 删除数)。删除失败记
// error 日志并继续——清理是尽力而为的留存面任务，不因单表失败中断另一表
// （下一轮重试）。
func (j *RetentionJob) RunOnce(ctx context.Context) (int64, int64) {
	if j.days <= 0 {
		return 0, 0
	}
	cutoff := j.now().AddDate(0, 0, -j.days)

	tracesDeleted, err := j.traces.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		slog.Error("guard retention: delete old traces", "error", err)
		tracesDeleted = 0
	} else if tracesDeleted > 0 {
		retentionDeletedRows.WithLabelValues("traces").Add(float64(tracesDeleted))
	}

	eventsDeleted, err := j.events.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		slog.Error("guard retention: delete old events", "error", err)
		eventsDeleted = 0
	} else if eventsDeleted > 0 {
		retentionDeletedRows.WithLabelValues("events").Add(float64(eventsDeleted))
	}

	if eventsDeleted > 0 || tracesDeleted > 0 {
		slog.Info("guard retention cleanup",
			"events_deleted", eventsDeleted, "traces_deleted", tracesDeleted, "cutoff", cutoff.Format(time.RFC3339))
	}
	return eventsDeleted, tracesDeleted
}

// RunLoop 阻塞运行：启动即清一次，随后按 interval 周期执行。ctx 取消即退。
func (j *RetentionJob) RunLoop(ctx context.Context) {
	j.RunOnce(ctx)
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.RunOnce(ctx)
		}
	}
}
