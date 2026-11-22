package dialect

// Prompt 护栏改造的迁移回归（规则范围 96、应用配置 97、审核服务 98、
// 独立护栏凭证映射 105）。与 m3_rule_versions_migration_test.go 同一套路：
// 执行仓库里真实的迁移文件，而不是 AutoMigrate 出的结构。
//
//   - 全新安装：按序执行到本批次最后一个版本不报错，且本批次之前的结构里
//     不包含 scope_mode/api_key_ids（重复列的故障类型）；
//   - 已部署升级：升级前写入的历史规则在升级后得到 scope_mode='all'、
//     api_key_ids 为空，保持「历史规则默认全部 Key」的行为；
//   - 回滚与再升级：down 按逆序移除四张表/两个列，再 up 可以恢复，
//     历史规则仍在且范围不变；
//   - 模型一致性：迁移建出的表能被 gorm 模型写入并读回。
//
// SQLite 在进程内执行。Postgres/MySQL 仅在可达时执行（PG_TEST_DSN /
// MYSQL_TEST_DSN 或默认测试实例），不可达时跳过并在日志里说明。

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/guardrail"
	gomysql "gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type guardBatchStep struct {
	ver  int
	slug string
}

type guardProfileMigDB struct {
	name     string
	db       *gorm.DB
	chainDir string
	prevTail int // 本批次之前链尾版本
	steps    []guardBatchStep
}

func (p guardProfileMigDB) applyUpTo(t *testing.T, maxVer int) {
	t.Helper()
	entries, err := os.ReadDir(p.chainDir)
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, ".up.sql") {
			continue
		}
		ver, perr := strconv.Atoi(strings.SplitN(n, "_", 2)[0])
		if perr != nil || ver > maxVer {
			continue
		}
		files = append(files, n)
	}
	sort.Strings(files)
	for _, f := range files {
		applyMigrations(t, p.db, migFile(t, p.chainDir+"/"+f), p.name+" "+f)
	}
}

func (p guardProfileMigDB) applyStep(t *testing.T, s guardBatchStep, down bool) {
	t.Helper()
	suffix := ".up.sql"
	if down {
		suffix = ".down.sql"
	}
	applyMigrations(t, p.db,
		migFile(t, fmt.Sprintf("%s/%06d_%s%s", p.chainDir, s.ver, s.slug, suffix)),
		fmt.Sprintf("%s %06d %s", p.name, s.ver, suffix))
}

func (p guardProfileMigDB) applyBatchUp(t *testing.T) {
	t.Helper()
	for _, s := range p.steps {
		p.applyStep(t, s, false)
	}
}

func (p guardProfileMigDB) applyBatchDown(t *testing.T) {
	t.Helper()
	for i := len(p.steps) - 1; i >= 0; i-- {
		p.applyStep(t, p.steps[i], true)
	}
}

// guardProfileArtifactCount 统计本批次生成的结构：4 张表 + 3 个列。
func guardProfileArtifactCount(t *testing.T, p guardProfileMigDB) (tables, columns int64) {
	t.Helper()
	tablesSQL := `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('security_profiles','security_profile_versions','security_profile_bindings','review_services')`
	columnsSQL := `SELECT COUNT(*) FROM information_schema.columns WHERE (table_name='guardrail_rules' AND column_name IN ('scope_mode','api_key_ids')) OR (table_name='guard_ingest_tokens' AND column_name='api_key_id')`
	switch p.name {
	case "sqlite":
		p.db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('security_profiles','security_profile_versions','security_profile_bindings','review_services')`).Scan(&tables)
		p.db.Raw(`SELECT (SELECT COUNT(*) FROM pragma_table_info('guardrail_rules') WHERE name IN ('scope_mode','api_key_ids')) + (SELECT COUNT(*) FROM pragma_table_info('guard_ingest_tokens') WHERE name='api_key_id')`).Scan(&columns)
	case "postgres", "mysql":
		p.db.Raw(tablesSQL).Scan(&tables)
		if p.name == "mysql" {
			p.db.Raw(columnsSQL + " AND table_schema=DATABASE()").Scan(&columns)
		} else {
			p.db.Raw(columnsSQL).Scan(&columns)
		}
	default:
		t.Fatalf("unknown dialect %q", p.name)
	}
	return tables, columns
}

func guardProfileSeedLegacyRule(t *testing.T, p guardProfileMigDB) {
	t.Helper()
	stmt := `INSERT INTO guardrail_rules (org_id, name, type, direction, enabled, config, severity, action)
		VALUES (1, 'legacy-rule', 'keyword_filter', 'request', true, '{"blocklist":["OLDKEY"]}', 'medium', 'block')`
	if err := p.db.Exec(stmt).Error; err != nil {
		t.Fatalf("seed legacy rule: %v", err)
	}
}

// guardProfileDialects 返回每个可达方言的干净库。SQLite 每次调用都新建。
func guardProfileDialects(t *testing.T) []guardProfileMigDB {
	t.Helper()
	steps := []guardBatchStep{
		{96, "guardrail_key_scope"},
		{97, "security_profiles"},
		{98, "review_services"},
		{105, "guard_ingest_key"},
	}
	out := []guardProfileMigDB{
		{name: "sqlite", db: freshMigrationDB(t), chainDir: "../../migrations/sqlite", prevTail: 21,
			steps: []guardBatchStep{{22, "guardrail_key_scope"}, {23, "security_profiles"}, {24, "review_services"}, {31, "guard_ingest_key"}}},
	}

	pgDSN := os.Getenv("PG_TEST_DSN")
	if pgDSN == "" {
		pgDSN = "postgres://crosslink:crosslink_test@localhost:15433/crosslink_test_pg?sslmode=disable"
	}
	if db, err := gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		dropAllTablesPG(t, db)
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		out = append(out, guardProfileMigDB{name: "postgres", db: db, chainDir: "../../migrations/postgres",
			prevTail: 95, steps: steps})
	} else {
		t.Logf("postgres test instance unreachable (%v) — postgres subtests will skip", err)
	}

	myDSN := os.Getenv("MYSQL_TEST_DSN")
	if myDSN == "" {
		myDSN = "root:crosslink_test@tcp(127.0.0.1:13307)/crosslink_test_mysql?charset=utf8mb4&parseTime=true&loc=UTC"
	}
	if db, err := gorm.Open(gomysql.Open(myDSN), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}); err == nil {
		dropAllTablesMySQL(t, db)
		sqlDB, _ := db.DB()
		t.Cleanup(func() { _ = sqlDB.Close() })
		out = append(out, guardProfileMigDB{name: "mysql", db: db, chainDir: "../../migrations/mysql",
			prevTail: 18, steps: []guardBatchStep{{19, "guardrail_key_scope"}, {20, "security_profiles"}, {21, "review_services"}, {28, "guard_ingest_key"}}})
	} else {
		t.Logf("mysql test instance unreachable (%v) — mysql subtests will skip", err)
	}
	return out
}

func TestPromptGuardrailMigrationChain(t *testing.T) {
	for _, dia := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dia+"/fresh_install", func(t *testing.T) {
			matched := false
			for _, p := range guardProfileDialects(t) {
				if p.name != dia {
					continue
				}
				matched = true
				p.applyUpTo(t, p.prevTail)
				if tables, columns := guardProfileArtifactCount(t, p); tables != 0 || columns != 0 {
					t.Fatalf("本批次之前不应存在新结构，tables=%d columns=%d（全新安装会撞重复列/重复表）", tables, columns)
				}
				p.applyBatchUp(t)
				if tables, columns := guardProfileArtifactCount(t, p); tables != 4 || columns != 3 {
					t.Fatalf("全新安装后结构不完整，tables=%d columns=%d", tables, columns)
				}
				t.Logf("%s: 全新安装全链通过，4 张表 + 3 个列就位", p.name)
			}
			if !matched {
				t.Skipf("%s 测试实例不可达，跳过", dia)
			}
		})

		t.Run(dia+"/deployed_upgrade_defaults_all", func(t *testing.T) {
			matched := false
			for _, p := range guardProfileDialects(t) {
				if p.name != dia {
					continue
				}
				matched = true
				p.applyUpTo(t, p.prevTail)
				guardProfileSeedLegacyRule(t, p)
				p.applyBatchUp(t)

				var row struct {
					ScopeMode string
					APIKeyIDs *string
				}
				if err := p.db.Raw(`SELECT scope_mode, api_key_ids FROM guardrail_rules WHERE name = 'legacy-rule'`).Scan(&row).Error; err != nil {
					t.Fatalf("read legacy rule: %v", err)
				}
				if row.ScopeMode != "all" {
					t.Fatalf("历史规则升级后应为 all，实际 %q", row.ScopeMode)
				}
				if row.APIKeyIDs != nil {
					t.Fatalf("历史规则的 api_key_ids 应为空，实际 %q", *row.APIKeyIDs)
				}
				t.Logf("%s: 历史规则升级后 scope_mode=all、api_key_ids 为空", p.name)
			}
			if !matched {
				t.Skipf("%s 测试实例不可达，跳过", dia)
			}
		})

		t.Run(dia+"/down_and_reup", func(t *testing.T) {
			matched := false
			for _, p := range guardProfileDialects(t) {
				if p.name != dia {
					continue
				}
				matched = true
				p.applyUpTo(t, p.prevTail)
				guardProfileSeedLegacyRule(t, p)
				p.applyBatchUp(t)

				p.applyBatchDown(t)
				if tables, columns := guardProfileArtifactCount(t, p); tables != 0 || columns != 0 {
					t.Fatalf("down 后结构应被移除，tables=%d columns=%d", tables, columns)
				}
				var rules int64
				p.db.Raw(`SELECT COUNT(*) FROM guardrail_rules`).Scan(&rules)
				if rules != 1 {
					t.Fatalf("down 不应删除规则行，实际 %d", rules)
				}

				p.applyBatchUp(t)
				if tables, columns := guardProfileArtifactCount(t, p); tables != 4 || columns != 3 {
					t.Fatalf("再升级后结构不完整，tables=%d columns=%d", tables, columns)
				}
				var row struct {
					ScopeMode string
					APIKeyIDs *string
				}
				if err := p.db.Raw(`SELECT scope_mode, api_key_ids FROM guardrail_rules WHERE name = 'legacy-rule'`).Scan(&row).Error; err != nil {
					t.Fatalf("re-upgrade read legacy rule: %v", err)
				}
				if row.ScopeMode != "all" {
					t.Fatalf("再升级后历史规则仍应为 all，实际 %q", row.ScopeMode)
				}
				t.Logf("%s: down/up 往返通过，规则保留且范围不变", p.name)
			}
			if !matched {
				t.Skipf("%s 测试实例不可达，跳过", dia)
			}
		})

		t.Run(dia+"/model_parity", func(t *testing.T) {
			matched := false
			for _, p := range guardProfileDialects(t) {
				if p.name != dia {
					continue
				}
				matched = true
				p.applyUpTo(t, p.prevTail)
				p.applyBatchUp(t)

				profile := guardrail.SecurityProfile{OrgID: 1, Revision: 1, ProfileContent: guardrail.ProfileContent{
					Name: "客服助手", Purpose: "查询订单", AllowedTasks: "查询订单", Forbidden: "修改金额", DataBoundary: "不发送证件号",
				}}
				if err := p.db.Create(&profile).Error; err != nil {
					t.Fatalf("create security profile: %v", err)
				}
				ver := guardrail.SecurityProfileVersion{ProfileID: profile.ID, Version: 1, Actor: "7",
					CreatedAt: time.Now().UTC().Truncate(time.Second), ProfileContent: profile.ProfileContent}
				if err := p.db.Create(&ver).Error; err != nil {
					t.Fatalf("create security profile version: %v", err)
				}
				if err := p.db.Create(&guardrail.SecurityProfileBinding{APIKeyID: 42, ProfileID: profile.ID}).Error; err != nil {
					t.Fatalf("create security profile binding: %v", err)
				}
				svc := guardrail.ReviewService{OrgID: 1, Name: "学生", Kind: "local", Endpoint: "http://10.0.0.8:18081",
					ModelVersion: "student-B", Threshold: 0.7, TimeoutMS: 1500}
				if err := p.db.Create(&svc).Error; err != nil {
					t.Fatalf("create review service: %v", err)
				}

				var got guardrail.SecurityProfile
				if err := p.db.Where("id = ?", profile.ID).First(&got).Error; err != nil {
					t.Fatalf("read security profile: %v", err)
				}
				if got.Name != "客服助手" || got.Purpose != "查询订单" || got.Published != 0 {
					t.Fatalf("profile round-trip mismatch: %+v", got)
				}
				var gotSvc guardrail.ReviewService
				if err := p.db.Where("id = ?", svc.ID).First(&gotSvc).Error; err != nil {
					t.Fatalf("read review service: %v", err)
				}
				if gotSvc.Endpoint != svc.Endpoint || gotSvc.TimeoutMS != 1500 || gotSvc.Threshold != 0.7 || gotSvc.ProviderModelID != 0 {
					t.Fatalf("review service round-trip mismatch: %+v", gotSvc)
				}
				if err := p.db.Create(&guardrail.SecurityProfileBinding{APIKeyID: 42, ProfileID: profile.ID}).Error; err == nil {
					t.Fatal("同一 Key 重复绑定应被主键拒绝")
				}
				t.Logf("%s: 模型写入/读回一致，单 Key 唯一绑定生效", p.name)
			}
			if !matched {
				t.Skipf("%s 测试实例不可达，跳过", dia)
			}
		})
	}
}
