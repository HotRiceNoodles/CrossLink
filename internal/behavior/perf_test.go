package behavior

// G0 门槛 6 性能基准（规划冻结 v4.0 §6）：env 门控（BEHAVIOR_PERF_TEST=1，
// KB_POISON_PERF_GATE 同款纪律）——默认全量测试跳过，显式启用才跑规格规模。
//
// 规格口径（§6）：单实例、1 万活跃 key、usage_logs 1000 万行/28 天、小时作业
// <60s（48h 重聚合+受影响实体快照重算+打分）、每日全量轮 ≤10min。
//
// 本地为**指示性**测量（SQLite 单文件、Windows 桌面盘）——CI 参考机（4 vCPU、
// MySQL/PG 容器）为终裁。种子策略：历史天 rollups 直插 + 近 48h usage_logs 真行
// ——小时门槛承载其真实成本中心（受影响实体快照重算的写放大）。基线窗口可用
// BEHAVIOR_PERF_DAYS 缩短（默认 14：快照写体积不变，仅基线读窗缩短；CI 可设 28）。
// BEHAVIOR_PERF_KEYS/ORGS 控制实体规模。回填成本=分片实测线性外推（标注估算）。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

func perfEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func TestBehaviorPerfSpecScale(t *testing.T) {
	if os.Getenv("BEHAVIOR_PERF_TEST") != "1" {
		t.Skip("spec-scale perf benchmark; set BEHAVIOR_PERF_TEST=1 (indicative locally, CI is final)")
	}
	keys := int64(perfEnvInt("BEHAVIOR_PERF_KEYS", 10000))
	orgs := int64(perfEnvInt("BEHAVIOR_PERF_ORGS", 1000))
	days := perfEnvInt("BEHAVIOR_PERF_DAYS", 14)
	hourGate := float64(perfEnvInt("BEHAVIOR_PERF_GATE_SEC", 60))
	dayGate := float64(perfEnvInt("BEHAVIOR_PERF_DAY_SEC", 600))

	db := testDB(t)
	agg := &Aggregator{DB: db}
	ctx := context.Background()
	now := time.Now().UTC()

	activeHours := func(k int64) [3]int {
		s := 8 + int(k)%12
		return [3]int{s, (s + 1) % 24, (s + 2) % 24}
	}
	hourActive := func(k int64, h int) bool {
		for _, x := range activeHours(k) {
			if x == h {
				return true
			}
		}
		return false
	}
	coreMetrics := []string{MetricRequestRate, MetricTokensIn, MetricTokensOut, MetricTokensRea, MetricCostCNY, MetricErrGw, MetricErrUp, MetricGrTrigger}

	// ── 种子 1：历史天 rollups 直插（key 层 + org 层）──
	t0 := time.Now()
	var rollupRows int64
	type rrow struct {
		et  string
		id  int64
		m   string
		ts  time.Time
		val float64
	}
	batch := make([]rrow, 0, 20000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			const perStmt = 500
			for start := 0; start < len(batch); start += perStmt {
				end := start + perStmt
				if end > len(batch) {
					end = len(batch)
				}
				var b strings.Builder
				b.WriteString("INSERT INTO behavior_rollups (entity_type, entity_id, metric, granularity, bucket_ts, value_num, value_den, sample_count, updated_at) VALUES ")
				args := make([]any, 0, perStmt*8)
				for i := start; i < end; i++ {
					if i > start {
						b.WriteByte(',')
					}
					b.WriteString("(?,?,?,'hour',?,?,?,?,?)")
					r := batch[i]
					args = append(args, r.et, r.id, r.m, r.ts, r.val, r.val, int64(r.val), now)
				}
				if err := tx.Exec(b.String(), args...).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		rollupRows += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for d := days; d >= 2; d-- {
		day := now.AddDate(0, 0, -d)
		for h := 0; h < 24; h++ {
			bucket := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, time.UTC)
			for k := int64(1); k <= keys; k++ {
				if !hourActive(k, h) {
					continue
				}
				for _, m := range coreMetrics {
					batch = append(batch, rrow{"key", k, m, bucket, 12})
				}
			}
			if h >= 8 && h <= 22 { // org 层：key 分散在 8..22 槽,org 全时活跃
				for o := int64(1); o <= orgs; o++ {
					for _, m := range coreMetrics {
						batch = append(batch, rrow{"org", o, m, bucket, 1500})
					}
				}
			}
			if err := flush(); err != nil {
				t.Fatalf("seed rollups: %v", err)
			}
		}
	}
	if err := flush(); err != nil {
		t.Fatalf("seed rollups: %v", err)
	}
	t.Logf("[perf] seed rollups: %d rows in %s (days=%d keys=%d orgs=%d)", rollupRows, time.Since(t0).Round(time.Millisecond), days, keys, orgs)

	// ── 种子 2：近 48h usage_logs 真行（小时作业的真实重聚合面）──
	t1s := time.Now()
	var usageRows int64
	{
		const rowsPerKeyHour = 12
		firstHour := now.Truncate(time.Hour).Add(-47 * time.Hour)
		lastHour := now.Truncate(time.Hour).Add(-time.Hour)
		for hh := firstHour; !hh.After(lastHour); hh = hh.Add(time.Hour) {
			h := hh.Hour()
			var rowArgs [][13]any
			for k := int64(1); k <= keys; k++ {
				if !hourActive(k, h) {
					continue
				}
				for i := 0; i < rowsPerKeyHour; i++ {
					at := hh.Add(time.Duration(i*4) * time.Minute)
					rowArgs = append(rowArgs, [13]any{
						fmt.Sprintf("p-%d-%d", k, at.UnixNano()), k, 1 + (k-1)%orgs, "openai", "gpt-x",
						100, 50, 10, 1.5, 200, 200, at, 200,
					})
				}
			}
			// 分块：≤2000 行/语句（26k 参数,低于 sqlite 32766 上限——单语句 31 万
			// 参数爆上限是第一版种子的死因）。
			const chunk = 2000
			if err := db.Transaction(func(tx *gorm.DB) error {
				for start := 0; start < len(rowArgs); start += chunk {
					end := start + chunk
					if end > len(rowArgs) {
						end = len(rowArgs)
					}
					var b strings.Builder
					b.WriteString("INSERT INTO usage_logs (request_id, api_key_id, org_id, route_type, model_used, input_tokens, output_tokens, reasoning_tokens, billable_cost, latency_ms, status_code, created_at, model_requested) VALUES ")
					args := make([]any, 0, chunk*13)
					for i := start; i < end; i++ {
						if i > start {
							b.WriteByte(',')
						}
						b.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?)")
						args = append(args, rowArgs[i][:]...)
					}
					if err := tx.Exec(b.String(), args...).Error; err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatalf("seed usage hour %s: %v", hh, err)
			}
			usageRows += int64(len(rowArgs))
		}
	}
	t.Logf("[perf] seed usage_logs(48h): %d rows in %s", usageRows, time.Since(t1s).Round(time.Millisecond))

	// ── 阶段 1：每日全量轮（全实体快照重算）──
	t2s := time.Now()
	allEnts, err := agg.activeEntities(ctx, 40)
	if err != nil {
		t.Fatalf("active entities: %v", err)
	}
	if err := agg.RecomputeBaselines(ctx, allEnts); err != nil {
		t.Fatalf("daily recompute: %v", err)
	}
	dailySec := time.Since(t2s).Seconds()
	t.Logf("[perf] DAILY full recompute: %d entities in %.1fs (gate ≤%.0fs)", len(allEnts), dailySec, dayGate)

	// ── 阶段 2a：小时作业·冷启动轮（48h 全量重聚合——信息项，不设门）──
	t3s := time.Now()
	if err := runHourlyJob(ctx, agg); err != nil {
		t.Fatalf("hourly job (cold): %v", err)
	}
	coldSec := time.Since(t3s).Seconds()
	t.Logf("[perf] HOURLY job COLD (full 48h re-agg): %.1fs (informational; steady-state is the gate)", coldSec)

	// ── 阶段 2b：小时作业·稳态轮（注入一个新小时的流量后测量——门槛对象）──
	{
		newHour := now.Truncate(time.Hour)
		var rowArgs [][13]any
		for k := int64(1); k <= keys; k++ {
			if !hourActive(k, newHour.Hour()) {
				continue
			}
			for i := 0; i < 12; i++ {
				at := newHour.Add(time.Duration(i*4) * time.Minute)
				rowArgs = append(rowArgs, [13]any{
					fmt.Sprintf("p-%d-%d", k, at.UnixNano()), k, 1 + (k-1)%orgs, "openai", "gpt-x",
					100, 50, 10, 1.5, 200, 200, at, 200,
				})
			}
		}
		const chunk = 2000
		if err := db.Transaction(func(tx *gorm.DB) error {
			for start := 0; start < len(rowArgs); start += chunk {
				end := start + chunk
				if end > len(rowArgs) {
					end = len(rowArgs)
				}
				var b strings.Builder
				b.WriteString("INSERT INTO usage_logs (request_id, api_key_id, org_id, route_type, model_used, input_tokens, output_tokens, reasoning_tokens, billable_cost, latency_ms, status_code, created_at, model_requested) VALUES ")
				args := make([]any, 0, chunk*13)
				for i := start; i < end; i++ {
					if i > start {
						b.WriteByte(',')
					}
					b.WriteString("(?,?,?,?,?,?,?,?,?,?,?,?,?)")
					args = append(args, rowArgs[i][:]...)
				}
				if err := tx.Exec(b.String(), args...).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed steady hour: %v", err)
		}
	}
	t5s := time.Now()
	if err := runHourlyJob(ctx, agg); err != nil {
		t.Fatalf("hourly job (steady): %v", err)
	}
	hourlySec := time.Since(t5s).Seconds()
	t.Logf("[perf] HOURLY job STEADY (1 new hour): %.1fs (gate <%.0fs) — indicative on sqlite/desktop; CI 4vCPU+MySQL/PG is final", hourlySec, hourGate)

	// ── 回填成本估算（3 个已知活跃小时分片实测均值线性外推,标注估算）──
	t4s := time.Now()
	yday := now.AddDate(0, 0, -1)
	sliceStart := time.Date(yday.Year(), yday.Month(), yday.Day(), 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if _, err := agg.ReaggregateHour(ctx, sliceStart.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("backfill slice: %v", err)
		}
	}
	perHour := time.Since(t4s).Seconds() / 3
	t.Logf("[perf] backfill ESTIMATE: %.2fs/active-hour measured → 28d×15 ≈ %.0fmin (budget: 30min/M rows)", perHour, perHour*28*15/60)

	if hourlySec >= hourGate {
		t.Errorf("HOURLY job %.1fs exceeds gate %.0fs (keys=%d days=%d) — CI 终裁;若 CI 同超,需优化快照写放大", hourlySec, hourGate, keys, days)
	}
	if dailySec >= dayGate {
		t.Errorf("DAILY recompute %.1fs exceeds gate %.0fs", dailySec, dayGate)
	}
	_ = model.BehaviorRollup{}
}
