package guardapp

import (
	"github.com/crosslink/internal/worker"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Guard ingest 指标。声明贴近唯一调用方（与 internal/guardrail/metrics.go
// 同一理由：远离调用方的指标永远导出零值）。/metrics 暴露端点在 Step 9 接线。
var (
	// ingestDroppedTotal 计异步递交被拒的 payload 数：检测队列与溢出额度
	// 同时打满（DD-4 背压策略：丢弃+计数，绝不反压客户网关）。一次递增
	// 意味着一段流量未被检测——这是旁路服务的告警面。
	ingestDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "cl_guard_ingest_dropped_total",
		Help: "Async ingest payloads dropped because the guard detection pool queue and overflow allowance were both exhausted. An increase means traffic went undetected (bypass semantics: drop and count, never backpressure).",
	})

	// detectDurationSeconds 分桶记录一次检测的端到端耗时（规范化+引擎+
	// 裁决映射，不含落库——落库走 worker 池不进客户路径）。mode 区分同步
	// （客户在等）与异步（预算更宽松），两者的 SLO 面不同。
	detectDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cl_guard_detect_duration_seconds",
		Help:    "End-to-end duration of one detection (normalize + engines + verdict mapping; persistence excluded). mode=sync serves the customer-facing verdict, mode=async runs on the worker pool.",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"mode"})

	// verdictsTotal 按裁决计数——would_block 命中率 =
	// verdicts_total{verdict="would_block"} / verdicts_total（全量分母，
	// pass 也计入）。error 是执行失败而非内容判定，单列不进命中率分母的
	// 提醒交给消费方（告警规则模板 M2 随控制台出）。
	verdictsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guard_verdicts_total",
		Help: "Detection verdicts by outcome. would_block rate = would_block / sum(all); error means detection execution failure, not a content verdict.",
	}, []string{"mode", "verdict"})

	// retentionDeletedRows 计留存清理实际删除的行数（table=events|traces）。
	// 长期为零且表在涨 = 清理未生效的巡检信号。
	retentionDeletedRows = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guard_retention_deleted_rows_total",
		Help: "Rows removed by the guard retention cleanup job, by table.",
	}, []string{"table"})

	// mirrorIngestTotal 按结果计镜像端点请求数（M2）：rejected 覆盖
	// 413/415/畸形报文（同时落 guard_events 覆盖缺口事件），dropped 是
	// 队列打满——两者与 ingest_dropped_total 同属"有流量未被检测"告警面。
	mirrorIngestTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cl_guard_mirror_total",
		Help: "Mirror-endpoint requests by result: accepted, duplicate (idempotent replay), dropped (queue saturated), rejected (oversize/unsupported encoding/malformed).",
	}, []string{"result"})
)

// registerQueueDepthGauge 把检测池的实时积压（队列 + 溢出在途）挂成 gauge。
// Run 装配时调用一次；池与指标同生命周期（进程级）。
func registerQueueDepthGauge(pool *worker.Pool) {
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "cl_guard_queue_depth",
		Help: "Current guard detection pool backlog: queued tasks plus detached tasks running under the overflow allowance.",
	}, func() float64 { return float64(pool.Depth()) })
}
