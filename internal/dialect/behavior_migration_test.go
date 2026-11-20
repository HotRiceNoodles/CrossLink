package dialect

// behavior 迁移回归（行为基线防护 M1+M2，docs/plans/2026-11-19-behavior-baseline-defense.md
// 冻结 v4.0）。与 TestGuardTablesMigration 同一纪律：按各方言链**真实顺序**执行全部
// .up.sql 后验证链尾新表对——链号已分叉（mysql 20 / sqlite 23 / pg 97），只追加不假设对齐。
//
//   - fresh install：全链过新版本干净通过（M1 四表 + M2 台账/容忍度）；
//   - gorm 模型 parity：经 model 包结构写入/读回；
//   - 唯一键语义：rollup 的 (entity,metric,bucket)、已见集的 (entity,kind,category)、
//     台账的 (fingerprint,merge_bucket)、容忍度的 (entity,metric) 冲突必须被拒绝；
//   - rollback/re-upgrade：down 六表全消、up 重建后可再写。
//
// SQLite 进程内跑；PG/MySQL 仅在可达时跑（PG_TEST_DSN / MYSQL_TEST_DSN 或默认
// 测试容器）——与 guard 链测试相同的环境约定。

import (
	"os"
	"testing"
	"time"

	"github.com/crosslink/internal/model"
	gomysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func behaviorMigDialects(t *testing.T) []guardMigDB {
	t.Helper()
	// 编号已让位于商业 overlay 迁移（合并树同目录）：overlay 链尾 sqlite 24 /
	// mysql 21 / pg 98（kb_poison/rag_ledger），社区 behavior 对在其上
	// （M3 指纹表再上一级：sqlite 27 / mysql 24 / pg 101）。
	out := []guardMigDB{
		{name: "sqlite", db: freshMigrationDB(t), chainDir: "../../migrations/sqlite", newVer: 27, newSlug: "behavior_fingerprints"},
	}
	out = append(out, optionalPGMigrationDB(t, guardMigDB{name: "postgres", chainDir: "../../migrations/postgres", newVer: 101, newSlug: "behavior_fingerprints"})...)
	out = append(out, optionalMySQLMigrationDB(t, guardMigDB{name: "mysql", chainDir: "../../migrations/mysql", newVer: 24, newSlug: "behavior_fingerprints"})...)
	return out
}

func TestBehaviorTablesMigration(t *testing.T) {
	for _, p := range behaviorMigDialects(t) {
		t.Run(p.name, func(t *testing.T) {
			// Fresh install：完整链到新对。
			p.applyChainUp(t, p.newVer)

			// 模型 parity：rollup（直方图 JSON）+ 快照 + 已见集写入读回。
			now := time.Now().UTC().Truncate(time.Hour)
			hist := `[[79,10],[103,2]]`
			num, den := 12.0, 12.0
			rl := &model.BehaviorRollup{
				EntityType: "key", EntityID: 1, Metric: "latency", Granularity: "day",
				BucketTs: now, ValueHist: &hist, SampleCount: 12,
			}
			if err := p.db.Create(rl).Error; err != nil {
				t.Fatalf("insert rollup: %v", err)
			}
			var gotRl model.BehaviorRollup
			if err := p.db.Where("entity_type = ? AND entity_id = ?", "key", 1).First(&gotRl).Error; err != nil {
				t.Fatalf("read back rollup: %v", err)
			}
			if gotRl.Granularity != "day" || gotRl.ValueHist == nil || *gotRl.ValueHist != hist {
				t.Fatalf("rollup parity: %+v", gotRl)
			}

			rate := &model.BehaviorRollup{
				EntityType: "key", EntityID: 1, Metric: "request_rate", Granularity: "hour",
				BucketTs: now, ValueNum: &num, ValueDen: &den, SampleCount: 12,
			}
			if err := p.db.Create(rate).Error; err != nil {
				t.Fatalf("insert request_rate rollup: %v", err)
			}
			// 唯一键：(entity, metric, bucket) 重复必须被拒绝。
			dup := &model.BehaviorRollup{
				EntityType: "key", EntityID: 1, Metric: "request_rate", Granularity: "hour",
				BucketTs: now, SampleCount: 1,
			}
			if err := p.db.Create(dup).Error; err == nil {
				t.Fatal("duplicate (entity, metric, bucket_ts) rollup must be rejected")
			}

			bl := &model.BehaviorBaseline{
				EntityType: "key", EntityID: 1, Metric: "request_rate", BucketKey: "9",
				P50: 10, P90: 20, P99: 25, EWMA: 11, EWMAVar: 4, ObsCount: 14,
				WindowStart: now.Add(-28 * 24 * time.Hour), WindowEnd: now,
				ModelVersion: 1,
			}
			if err := p.db.Create(bl).Error; err != nil {
				t.Fatalf("insert baseline: %v", err)
			}

			seen := &model.BehaviorSeenCategory{
				EntityType: "key", EntityID: 1, Kind: "model", Category: "gpt-x",
				FirstSeenAt: now, LastSeenAt: now,
			}
			if err := p.db.Create(seen).Error; err != nil {
				t.Fatalf("insert seen category: %v", err)
			}
			if err := p.db.Create(&model.BehaviorSeenCategory{
				EntityType: "key", EntityID: 1, Kind: "model", Category: "gpt-x",
				FirstSeenAt: now, LastSeenAt: now,
			}).Error; err == nil {
				t.Fatal("duplicate (entity, kind, category) must be rejected")
			}

			lock := &model.BehaviorJobLock{Job: "behavior_hourly", OwnerToken: "owner-a", Fence: 1, LeaseUntil: now.Add(time.Minute)}
			if err := p.db.Create(lock).Error; err != nil {
				t.Fatalf("insert job lock: %v", err)
			}
			if err := p.db.Create(&model.BehaviorJobLock{Job: "behavior_hourly", OwnerToken: "owner-b", LeaseUntil: now}).Error; err == nil {
				t.Fatal("duplicate job PK must be rejected")
			}

			// M2：台账 + 容忍度。合并唯一键 (fingerprint, merge_bucket) 只作并发兜底（D16）。
			ev := &model.BehaviorAnomaly{
				EventFingerprint: "fp-test-1", MergeBucket: now.Unix() / 14400,
				EntityType: "key", EntityID: 1, Score: 88, ScorePeak: 88, Band: "anomalous",
				BaselineModelVer: 1, FirstSeenAt: now, LastHitAt: now, HitCount: 1,
				WindowStart: now, WindowEnd: now.Add(time.Hour), Status: "open",
			}
			if err := p.db.Create(ev).Error; err != nil {
				t.Fatalf("insert anomaly: %v", err)
			}
			if err := p.db.Create(&model.BehaviorAnomaly{
				EventFingerprint: "fp-test-1", MergeBucket: now.Unix() / 14400,
				EntityType: "key", EntityID: 1, FirstSeenAt: now, LastHitAt: now,
				WindowStart: now, WindowEnd: now, Status: "open",
			}).Error; err == nil {
				t.Fatal("duplicate (fingerprint, merge_bucket) must be rejected")
			}
			tol := &model.BehaviorTolerance{EntityType: "key", EntityID: 1, Metric: "request_rate", Multiplier: 1.5, LastFeedbackAt: now}
			if err := p.db.Create(tol).Error; err != nil {
				t.Fatalf("insert tolerance: %v", err)
			}
			if err := p.db.Create(&model.BehaviorTolerance{EntityType: "key", EntityID: 1, Metric: "request_rate", LastFeedbackAt: now}).Error; err == nil {
				t.Fatal("duplicate (entity, metric) tolerance must be rejected")
			}

			// M3：指纹候选库——(entity, dimension_set) 唯一去重。
			fpdims := "request_rate,tokens"
			fp := &model.BehaviorFingerprint{
				EntityType: "key", EntityID: 1, DimensionSet: fpdims,
				Template: `[{"metric":"request_rate","s":100}]`, HitCount: 1, Status: "candidate",
			}
			if err := p.db.Create(fp).Error; err != nil {
				t.Fatalf("insert fingerprint: %v", err)
			}
			if err := p.db.Create(&model.BehaviorFingerprint{
				EntityType: "key", EntityID: 1, DimensionSet: fpdims,
				Template: `[]`, Status: "candidate",
			}).Error; err == nil {
				t.Fatal("duplicate (entity, dimension_set) fingerprint must be rejected")
			}

			// Rollback：链尾对（behavior_fingerprints）down 只删 M3 表——M1/M2 六表必须
			// 仍在（链式回滚语义）；re-up 后可再写。
			p.applyOne(t, true)
			if !p.tableGone(t, "behavior_fingerprints") {
				t.Fatal("behavior_fingerprints still present after down")
			}
			for _, table := range []string{"behavior_rollups", "behavior_baselines", "behavior_job_locks", "behavior_seen_categories", "behavior_anomalies", "behavior_tolerances"} {
				if p.tableGone(t, table) {
					t.Fatalf("%s must survive the M3 pair's down", table)
				}
			}
			p.applyOne(t, false)
			if err := p.db.Create(&model.BehaviorFingerprint{
				EntityType: "org", EntityID: 10, DimensionSet: "cost",
				Template: `[{"metric":"cost","s":70}]`, HitCount: 1, Status: "candidate",
			}).Error; err != nil {
				t.Fatalf("insert fingerprint after re-up: %v", err)
			}
		})
	}
}

// optionalPGMigrationDB / optionalMySQLMigrationDB 是 guard 链测试里可选方言装配
// 的同款实现（DSN 可达才加入、schema 清空）；不复用 guard 版是因为其装配内联在
// guardMigDialects 里（surgical change：不重构既有测试，此处独立实现）。
func optionalPGMigrationDB(t *testing.T, p guardMigDB) []guardMigDB {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://crosslink:crosslink_test@localhost:15433/crosslink_test_pg?sslmode=disable"
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Logf("postgres test instance unreachable (%v) — postgres subtests will skip", err)
		return nil
	}
	db.Exec("DROP SCHEMA public CASCADE")
	db.Exec("CREATE SCHEMA public")
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	p.db = db
	return []guardMigDB{p}
}

func optionalMySQLMigrationDB(t *testing.T, p guardMigDB) []guardMigDB {
	t.Helper()
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		dsn = "root:crosslink_test@tcp(127.0.0.1:13307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
	}
	db, err := gorm.Open(gomysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Logf("mysql test instance unreachable (%v) — mysql subtests will skip", err)
		return nil
	}
	dropAllTablesMySQL(t, db)
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	p.db = db
	return []guardMigDB{p}
}
