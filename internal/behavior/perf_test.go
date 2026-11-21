package behavior

// G0 门槛 6 性能基准（规划冻结 v4.0 §6）：env 门控（BEHAVIOR_PERF_TEST=1，
// KB_POISON_PERF_GATE 同款纪律）——默认全量测试跳过，显式启用才跑。
//
// 模式：
//   - 快模式（默认）：历史 rollups 直插 + 近 48h usage 真行 —— 快速指示性检查。
//   - 全量规格模式（BEHAVIOR_PERF_FULL=1）：**完整 28 天 usage_logs（~1000 万行，
//     冻结规格）+ 真实回填路径**（逐小时 ReaggregateHour 而非直插）——门槛证据
//     的规格口径；本机 sqlite 一次 ~40 分钟，CI 目标环境必跑此模式。
//   - 方言（BEHAVIOR_PERF_DSN）：mysql://DSN 或 postgres://DSN —— 目标环境
//     （冻结规格=MySQL/PG 容器）入口；不设则本机 sqlite。远端库由 AutoMigrate
//     建表（仅基准用；生产 schema 以迁移链为准——若 CI 已跑迁移，此处近似幂等）。
//
// 规格口径（§6）：单实例、1 万活跃 key、1000 万行/28 天、小时作业（稳态）<60s、
// 每日全量轮 ≤10min、回填 ≤30min/百万行。门槛只钉**稳态小时作业**——冷启动/回填
// 属一次性成本，单列报告。
//
// 旋钮：BEHAVIOR_PERF_KEYS(10000)/ORGGS(1000)/DAYS(快模式 14)/GATE_SEC(60)/
// DAY_SEC(600)。归因依赖 RunHourlyCycle/runHourlyJob 的分阶段计时日志。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	gomysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	sqliteDialector "github.com/glebarez/sqlite"

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

// perfOpenDB 打开基准库：BEHAVIOR_PERF_DSN（目标方言）> BEHAVIOR_PERF_DB（sqlite
// 文件复用——存在即跳过种子/回填,迭代提速）> TempDir 新库。
func perfOpenDB(t *testing.T) (*gorm.DB, bool) {
	t.Helper()
	if p := os.Getenv("BEHAVIOR_PERF_DB"); p != "" && os.Getenv("BEHAVIOR_PERF_DSN") == "" {
		if _, statErr := os.Stat(p); statErr == nil {
			db, err := gorm.Open(sqliteOpen(p), &gorm.Config{})
			if err != nil {
				t.Fatalf("open kept perf db: %v", err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { _ = sqlDB.Close() }) // 不删文件——复用
			return db, true
		}
		db, err := gorm.Open(sqliteOpen(p), &gorm.Config{})
		if err != nil {
			t.Fatalf("create perf db: %v", err)
		}
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		if err := db.AutoMigrate(
			&model.UsageLog{}, &model.BehaviorRollup{}, &model.BehaviorBaseline{},
			&model.BehaviorJobLock{}, &model.BehaviorSeenCategory{},
			&model.BehaviorAnomaly{}, &model.BehaviorTolerance{}, &model.BehaviorFingerprint{},
		); err != nil {
			t.Fatalf("automigrate perf schema: %v", err)
		}
		return db, false
	}
	dsn := os.Getenv("BEHAVIOR_PERF_DSN")
	if dsn == "" {
		return testDB(t), false
	}
	var dia gorm.Dialector
	switch {
	case strings.HasPrefix(dsn, "mysql://"):
		dia = gomysql.Open(strings.TrimPrefix(dsn, "mysql://"))
	default:
		dia = postgres.Open(strings.TrimPrefix(dsn, "postgres://"))
	}
	db, err := gorm.Open(dia, &gorm.Config{})
	if err != nil {
		t.Fatalf("open perf DSN: %v", err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(
		&model.UsageLog{}, &model.BehaviorRollup{}, &model.BehaviorBaseline{},
		&model.BehaviorJobLock{}, &model.BehaviorSeenCategory{},
		&model.BehaviorAnomaly{}, &model.BehaviorTolerance{}, &model.BehaviorFingerprint{},
	); err != nil {
		t.Fatalf("automigrate perf schema: %v", err)
	}
	return db, false
}

// sqliteOpen 是 glebarez 驱动的本地别名（避免与 DSN 分支的驱动导入混淆）。
func sqliteOpen(path string) gorm.Dialector { return sqliteDialector.Open(path) }

// seedUsageHours 批量插入给定小时列表的用量行（每活跃 key 12 行），返回行数。
func seedUsageHours(t *testing.T, db *gorm.DB, keys, orgs int64, hours []time.Time) int64 {
	t.Helper()
	var total int64
	const chunk = 2000 // ≤2000 行/语句（26k 参数,低于 sqlite 32766 上限）
	for _, hh := range hours {
		h := hh.Hour()
		var rowArgs [][13]any
		for k := int64(1); k <= keys; k++ {
			if !perfHourActive(k, h) {
				continue
			}
			for i := 0; i < 12; i++ {
				at := hh.Add(time.Duration(i*4) * time.Minute)
				rowArgs = append(rowArgs, [13]any{
					fmt.Sprintf("p-%d-%d", k, at.UnixNano()), k, 1 + (k-1)%orgs, "openai", "gpt-x",
					100, 50, 10, 1.5, 200, 200, at, 200,
				})
			}
		}
		if len(rowArgs) == 0 {
			continue
		}
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
		total += int64(len(rowArgs))
	}
	return total
}

// perfHourActive：key 活跃于 (k%24) 起的 3 个连续小时（每天 36 行）。轮换铺满
// 24 小时——任何墙钟时刻接跑,当前小时都有活跃 key（稳态测量依赖此性质）。
func perfHourActive(k int64, h int) bool {
	s := int(k) % 24
	for _, x := range []int{s, (s + 1) % 24, (s + 2) % 24} {
		if x == h {
			return true
		}
	}
	return false
}

func TestBehaviorPerfSpecScale(t *testing.T) {
	if os.Getenv("BEHAVIOR_PERF_TEST") != "1" {
		t.Skip("spec-scale perf benchmark; set BEHAVIOR_PERF_TEST=1 (BEHAVIOR_PERF_FULL=1 for frozen-spec full run, BEHAVIOR_PERF_DSN for target dialect)")
	}
	full := os.Getenv("BEHAVIOR_PERF_FULL") == "1"
	keys := int64(perfEnvInt("BEHAVIOR_PERF_KEYS", 10000))
	orgs := int64(perfEnvInt("BEHAVIOR_PERF_ORGS", 1000))
	days := perfEnvInt("BEHAVIOR_PERF_DAYS", 14)
	if full {
		days = perfEnvInt("BEHAVIOR_PERF_DAYS", 28) // 冻结规格 28 天
	}
	hourGate := float64(perfEnvInt("BEHAVIOR_PERF_GATE_SEC", 60))
	dayGate := float64(perfEnvInt("BEHAVIOR_PERF_DAY_SEC", 600))

	db, reusedDB := perfOpenDB(t)
	agg := &Aggregator{DB: db}
	ctx := context.Background()
	now := time.Now().UTC()
	mode := "fast(sqlite,rollups-direct)"
	if full {
		mode = "FULL-SPEC(real backfill)"
	}
	if dsn := os.Getenv("BEHAVIOR_PERF_DSN"); dsn != "" {
		mode += " dsn=" + dsn
	}
	t.Logf("[perf] mode=%s keys=%d orgs=%d days=%d", mode, keys, orgs, days)

	if full && reusedDB {
		t.Logf("[perf] reusing kept DB %s (seed+backfill skipped)", os.Getenv("BEHAVIOR_PERF_DB"))
	} else if full {
		// ── 全量规格：完整 28 天 usage + 真实回填 ──
		t0 := time.Now()
		var hours []time.Time
		for d := days; d >= 1; d-- {
			day := now.AddDate(0, 0, -d)
			for h := 0; h < 24; h++ {
				hh := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, time.UTC)
				if !hh.Before(now.Truncate(time.Hour)) {
					continue
				}
				hours = append(hours, hh)
			}
		}
		rows := seedUsageHours(t, db, keys, orgs, hours)
		t.Logf("[perf] seed usage_logs(%dd): %d rows in %s", days, rows, time.Since(t0).Round(time.Second))

		tb := time.Now()
		aggB := &Aggregator{DB: db, Now: func() time.Time { return now }}
		nActive := 0
		for _, hh := range hours {
			if _, err := aggB.ReaggregateHour(ctx, hh); err != nil {
				t.Fatalf("backfill %s: %v", hh, err)
			}
			nActive++
		}
		for d := days; d >= 0; d-- {
			if err := aggB.ReaggregateDayHistograms(ctx, now.AddDate(0, 0, -d)); err != nil {
				t.Fatalf("backfill dayhist: %v", err)
			}
		}
		backfillSec := time.Since(tb).Seconds()
		t.Logf("[perf] BACKFILL (real re-agg): %d hours in %.1fs → %.2fs/hour (budget 30min/M rows)", nActive, backfillSec, backfillSec/float64(nActive))
	} else {
		// ── 快模式：历史 rollups 直插 + 近 48h usage ──
		t0 := time.Now()
		coreMetrics := []string{MetricRequestRate, MetricTokensIn, MetricTokensOut, MetricTokensRea, MetricCostCNY, MetricErrGw, MetricErrUp, MetricGrTrigger}
		type rrow struct {
			et  string
			id  int64
			m   string
			ts  time.Time
			val float64
		}
		batch := make([]rrow, 0, 20000)
		var rollupRows int64
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
					if !perfHourActive(k, h) {
						continue
					}
					for _, m := range coreMetrics {
						batch = append(batch, rrow{"key", k, m, bucket, 12})
					}
				}
				if h >= 0 {
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
		t.Logf("[perf] seed rollups: %d rows in %s", rollupRows, time.Since(t0).Round(time.Millisecond))

		t1s := time.Now()
		var hours []time.Time
		for hh := now.Truncate(time.Hour).Add(-47 * time.Hour); hh.Before(now.Truncate(time.Hour)); hh = hh.Add(time.Hour) {
			hours = append(hours, hh)
		}
		rows := seedUsageHours(t, db, keys, orgs, hours)
		t.Logf("[perf] seed usage_logs(48h): %d rows in %s", rows, time.Since(t1s).Round(time.Millisecond))
	}

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

	// ── 阶段 2a：小时作业·冷启动轮（信息项）──
	t3s := time.Now()
	if err := runHourlyJob(ctx, agg); err != nil {
		t.Fatalf("hourly job (cold): %v", err)
	}
	coldSec := time.Since(t3s).Seconds()
	t.Logf("[perf] HOURLY job COLD: %.1fs (informational; steady-state is the gate)", coldSec)

	// ── 阶段 2b：小时作业·稳态轮（注入当前小时新流量后测量——门槛对象；
	// 轮换铺满 24h 保证任何接跑时刻当前小时都有活跃 key）──
	seeded := seedUsageHours(t, db, keys, orgs, []time.Time{now.Truncate(time.Hour)})
	if seeded == 0 {
		t.Fatal("steady-hour seeding produced 0 rows (rotation should cover all 24 hours)")
	}
	t5s := time.Now()
	if err := runHourlyJob(ctx, agg); err != nil {
		t.Fatalf("hourly job (steady): %v", err)
	}
	hourlySec := time.Since(t5s).Seconds()
	t.Logf("[perf] HOURLY job STEADY (1 new hour, %d rows): %.1fs (gate <%.0fs)", seeded, hourlySec, hourGate)

	if hourlySec >= hourGate {
		t.Errorf("HOURLY steady %.1fs exceeds gate %.0fs (keys=%d days=%d mode=%s) — CI 终裁;若 CI 同超,需优化快照写放大", hourlySec, hourGate, keys, days, mode)
	}
	if dailySec >= dayGate {
		t.Errorf("DAILY recompute %.1fs exceeds gate %.0fs", dailySec, dayGate)
	}
}
