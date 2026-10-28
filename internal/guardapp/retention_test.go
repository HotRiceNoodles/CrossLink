package guardapp

import (
	"context"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// newRetentionDB 复用真实迁移链（guard_events/guard_traces 时间列 DATETIME，
// DeleteOlderThan 的 created_at 比较走真实索引语义）。
func newRetentionDB(t *testing.T) (*repository.GuardEventRepo, *repository.GuardTraceRepo, *model.GuardEvent, *model.GuardTrace) {
	t.Helper()
	db, _, _, _ := newTestApp(t)
	events := repository.NewGuardEventRepo(db)
	traces := repository.NewGuardTraceRepo(db)
	ctx := context.Background()

	// 老/新各一行：旧行 CreatedAt 注入为 91 天前，新行当下。
	old := &model.GuardEvent{TraceID: "ret-old", OrgID: 1, Direction: "request", Action: "log",
		CreatedAt: time.Now().AddDate(0, 0, -91)}
	fresh := &model.GuardEvent{TraceID: "ret-new", OrgID: 1, Direction: "request", Action: "log",
		CreatedAt: time.Now().Add(-time.Hour)}
	for _, e := range []*model.GuardEvent{old, fresh} {
		if err := events.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	oldTr := &model.GuardTrace{TraceID: "ret-old", OrgID: 1, Status: model.GuardTraceComplete,
		Verdict: model.GuardVerdictPass, CreatedAt: time.Now().AddDate(0, 0, -91)}
	freshTr := &model.GuardTrace{TraceID: "ret-new", OrgID: 1, Status: model.GuardTraceOpen,
		Verdict: model.GuardVerdictPending, CreatedAt: time.Now().Add(-time.Hour)}
	for _, tr := range []*model.GuardTrace{oldTr, freshTr} {
		if err := traces.Create(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	return events, traces, fresh, freshTr
}

// 时间注入：RunOnce 用注入时钟算 cutoff，删 91 天前的行、留新行；
// 删除量计数进 cl_guard_retention_deleted_rows_total。
func TestRetentionRunOnceDeletesExpiredOnly(t *testing.T) {
	events, traces, fresh, freshTr := newRetentionDB(t)
	job := NewRetentionJob(events, traces, 90)
	fakeNow := time.Now().Add(time.Hour) // 时钟拨快一小时：cutoff 仍只覆盖旧行
	job.now = func() time.Time { return fakeNow }

	evBefore := testutil.ToFloat64(retentionDeletedRows.WithLabelValues("events"))
	trBefore := testutil.ToFloat64(retentionDeletedRows.WithLabelValues("traces"))

	nEv, nTr := job.RunOnce(context.Background())
	if nEv != 1 || nTr != 1 {
		t.Fatalf("deleted counts: events=%d traces=%d (want 1/1)", nEv, nTr)
	}
	ctx := context.Background()
	if rows, err := events.ListByTrace(ctx, 1, "ret-old"); err != nil || len(rows) != 0 {
		t.Fatalf("old events must be gone: %v %d", err, len(rows))
	}
	if rows, err := events.ListByTrace(ctx, 1, "ret-new"); err != nil || len(rows) != 1 {
		t.Fatalf("fresh events must survive: %v %d", err, len(rows))
	}
	if _, err := traces.FindByOrgTrace(ctx, 1, "ret-old"); err == nil {
		t.Fatal("old trace must be gone")
	}
	if _, err := traces.FindByOrgTrace(ctx, 1, "ret-new"); err != nil {
		t.Fatalf("fresh trace must survive: %v", err)
	}
	_ = fresh
	_ = freshTr

	if got := testutil.ToFloat64(retentionDeletedRows.WithLabelValues("events")) - evBefore; got != 1 {
		t.Fatalf("events metric delta: %v", got)
	}
	if got := testutil.ToFloat64(retentionDeletedRows.WithLabelValues("traces")) - trBefore; got != 1 {
		t.Fatalf("traces metric delta: %v", got)
	}
}

// retention_days<=0 显式关闭：不删任何行。
func TestRetentionDisabled(t *testing.T) {
	events, traces, _, _ := newRetentionDB(t)
	job := NewRetentionJob(events, traces, 0)
	if nEv, nTr := job.RunOnce(context.Background()); nEv != 0 || nTr != 0 {
		t.Fatalf("disabled job must not delete: %d/%d", nEv, nTr)
	}
	if rows, err := events.ListByTrace(context.Background(), 1, "ret-old"); err != nil || len(rows) != 1 {
		t.Fatalf("disabled job must keep rows: %v %d", err, len(rows))
	}
}

// RunLoop：ctx 取消即退（启动即清一次在 TestRetentionRunOnce 已覆盖语义）。
func TestRetentionRunLoopStopsOnCancel(t *testing.T) {
	events, traces, _, _ := newRetentionDB(t)
	job := NewRetentionJob(events, traces, 90)
	job.interval = 24 * time.Hour // 长周期：退出只可能来自 ctx 取消
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { job.RunLoop(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not exit on cancel")
	}
}
