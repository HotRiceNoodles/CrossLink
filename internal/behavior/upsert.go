package behavior

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/crosslink/internal/model"
)

// 幂等 upsert 的方言分支（规划冻结 v4.0 D9）。
//
// 冲突目标只用简单唯一键列，三种语法均为各自标准形态：
//
//	postgres/kingbase: INSERT ... ON CONFLICT (cols) DO UPDATE SET c = EXCLUDED.c
//	mysql:             INSERT ... ON DUPLICATE KEY UPDATE c = VALUES(c)
//	sqlite:            INSERT ... ON CONFLICT (cols) DO UPDATE SET c = excluded.c
//
// 占位符：pg 系用 $N（DataLens 的教训——gorm 原生 SQL 不会为 pg 把 ? 重写为 $N，
// 故按方言显式生成）；mysql/sqlite 用 ?。kingbase 走 pg 驱动，Dialector.Name() == "postgres"。

const upsertBatch = 200

func isPostgresFamily(db *gorm.DB) bool {
	switch db.Dialector.Name() {
	case "postgres":
		return true
	default:
		return false
	}
}

func ph(db *gorm.DB, i int) string {
	if isPostgresFamily(db) {
		return fmt.Sprintf("$%d", i)
	}
	return "?"
}

// upsertSQL 生成多行 upsert 语句。cols 为插入列（每行同序），conflict 为冲突
// 目标列，updates 为冲突时整列替换的列（本模块所有 upsert 的语义都是“按窗口
// 重算、整值覆盖”，无增量列）。
func upsertSQL(db *gorm.DB, table string, cols, conflict, updates []string, rows int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(cols, ", "))
	b.WriteString(") VALUES ")
	idx := 0
	for r := 0; r < rows; r++ {
		if r > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(")
		for c := range cols {
			if c > 0 {
				b.WriteString(", ")
			}
			idx++
			b.WriteString(ph(db, idx))
		}
		b.WriteString(")")
	}
	if isPostgresFamily(db) {
		b.WriteString(" ON CONFLICT (")
		b.WriteString(strings.Join(conflict, ", "))
		b.WriteString(") DO UPDATE SET ")
		for i, c := range updates {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%s = EXCLUDED.%s", c, c))
		}
	} else if db.Dialector.Name() == "mysql" {
		b.WriteString(" ON DUPLICATE KEY UPDATE ")
		for i, c := range updates {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%s = VALUES(%s)", c, c))
		}
	} else { // sqlite
		b.WriteString(" ON CONFLICT (")
		b.WriteString(strings.Join(conflict, ", "))
		b.WriteString(") DO UPDATE SET ")
		for i, c := range updates {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(fmt.Sprintf("%s = excluded.%s", c, c))
		}
	}
	return b.String()
}

// UpsertRollups 批量幂等写 rollup 事实行（唯一键 entity+metric+bucket_ts 整值覆盖）。
// 整批包一个事务：逐语句 autocommit 在 sqlite 上是 fsync 风暴（规格基准实测是
// 小时作业超线的主因之一）；部分失败保留旧值，下一轮重算覆盖——幂等语义不变。
func UpsertRollups(ctx context.Context, db *gorm.DB, rows []model.BehaviorRollup) error {
	if len(rows) == 0 {
		return nil
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(rows); start += upsertBatch {
			end := start + upsertBatch
			if end > len(rows) {
				end = len(rows)
			}
			batch := rows[start:end]
			sql := upsertSQL(db, "behavior_rollups",
				[]string{"entity_type", "entity_id", "metric", "granularity", "bucket_ts", "value_num", "value_den", "value_hist", "sample_count", "updated_at"},
				[]string{"entity_type", "entity_id", "metric", "bucket_ts"},
				[]string{"granularity", "value_num", "value_den", "value_hist", "sample_count", "updated_at"},
				len(batch))
			now := time.Now().UTC()
			args := make([]any, 0, len(batch)*10)
			for i := range batch {
				r := &batch[i]
				ts := r.BucketTs.UTC()
				vn, vd, vh := nullableNum(r.ValueNum), nullableNum(r.ValueDen), nullableStr(r.ValueHist)
				args = append(args, r.EntityType, r.EntityID, r.Metric, r.Granularity, ts, vn, vd, vh, r.SampleCount, now)
			}
			if err := tx.Exec(sql, args...).Error; err != nil {
				return fmt.Errorf("upsert behavior_rollups: %w", err)
			}
		}
		return nil
	})
}

// UpsertBaselines 批量幂等写基线快照（每轮全量重算覆盖，D8）。整批一个事务
// （快照写放大是小时作业的成本中心——逐语句 autocommit 实测超线的另一主因；
// 部分失败保留旧快照，下轮覆盖）。
func UpsertBaselines(ctx context.Context, db *gorm.DB, rows []model.BehaviorBaseline) error {
	if len(rows) == 0 {
		return nil
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for start := 0; start < len(rows); start += upsertBatch {
			end := start + upsertBatch
			if end > len(rows) {
				end = len(rows)
			}
			batch := rows[start:end]
			sql := upsertSQL(db, "behavior_baselines",
				[]string{"entity_type", "entity_id", "metric", "bucket_key", "p50", "p90", "p99", "ewma", "ewma_var", "obs_count", "window_start", "window_end", "model_version", "computed_at"},
				[]string{"entity_type", "entity_id", "metric", "bucket_key"},
				[]string{"p50", "p90", "p99", "ewma", "ewma_var", "obs_count", "window_start", "window_end", "model_version", "computed_at"},
				len(batch))
			now := time.Now().UTC()
			args := make([]any, 0, len(batch)*14)
			for i := range batch {
				r := &batch[i]
				args = append(args, r.EntityType, r.EntityID, r.Metric, r.BucketKey, r.P50, r.P90, r.P99, r.EWMA, r.EWMAVar, r.ObsCount, r.WindowStart.UTC(), r.WindowEnd.UTC(), r.ModelVersion, now)
			}
			if err := tx.Exec(sql, args...).Error; err != nil {
				return fmt.Errorf("upsert behavior_baselines: %w", err)
			}
		}
		return nil
	})
}

// UpsertSeenCategories 幂等写已见集：新类别插入，已知类别仅刷新 last_seen_at。
func UpsertSeenCategories(ctx context.Context, db *gorm.DB, rows []model.BehaviorSeenCategory) error {
	for start := 0; start < len(rows); start += upsertBatch {
		end := start + upsertBatch
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		sql := upsertSQL(db, "behavior_seen_categories",
			[]string{"entity_type", "entity_id", "kind", "category", "first_seen_at", "last_seen_at"},
			[]string{"entity_type", "entity_id", "kind", "category"},
			[]string{"last_seen_at"},
			len(batch))
		args := make([]any, 0, len(batch)*6)
		for i := range batch {
			r := &batch[i]
			args = append(args, r.EntityType, r.EntityID, r.Kind, r.Category, r.FirstSeenAt.UTC(), r.LastSeenAt.UTC())
		}
		if err := db.WithContext(ctx).Exec(sql, args...).Error; err != nil {
			return fmt.Errorf("upsert behavior_seen_categories: %w", err)
		}
	}
	return nil
}

// UpsertTolerance 幂等写单条容忍度乘子（反馈闭环落点，D10/D14）。
// created_at 只在首插给值（裸 INSERT 不走 gorm 的时间戳自动填充，AutoMigrate
// 建表下该列无 DB 默认值——漏列违反 NOT NULL）。
func UpsertTolerance(ctx context.Context, db *gorm.DB, row model.BehaviorTolerance) error {
	now := time.Now().UTC()
	sql := upsertSQL(db, "behavior_tolerances",
		[]string{"entity_type", "entity_id", "metric", "multiplier", "last_feedback_at", "created_at", "updated_at"},
		[]string{"entity_type", "entity_id", "metric"},
		[]string{"multiplier", "last_feedback_at", "updated_at"},
		1)
	if err := db.WithContext(ctx).Exec(sql,
		row.EntityType, row.EntityID, row.Metric, row.Multiplier, row.LastFeedbackAt.UTC(), now, now).Error; err != nil {
		return fmt.Errorf("upsert behavior_tolerances: %w", err)
	}
	return nil
}

func nullableNum(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// translateSQL 把手写 SQL 的顺序 ? 占位符改写为 $N（仅 postgres 系；gorm 原生
// 语句不会为 Raw/Exec 的字面 SQL 做这件事——DataLens U-11 的教训）。mysql/sqlite
// 原样返回。
func translateSQL(db *gorm.DB, sql string) string {
	if !isPostgresFamily(db) {
		return sql
	}
	var b strings.Builder
	n := 0
	for {
		i := strings.Index(sql, "?")
		if i < 0 {
			b.WriteString(sql)
			return b.String()
		}
		b.WriteString(sql[:i])
		n++
		fmt.Fprintf(&b, "$%d", n)
		sql = sql[i+1:]
	}
}

// raw 是带占位符翻译的 Raw 查询入口（聚合/流式查询统一走这里，D9）。
func raw(db *gorm.DB, ctx context.Context, sql string, args ...any) *gorm.DB {
	return db.WithContext(ctx).Raw(translateSQL(db, sql), args...)
}

// exec 是带占位符翻译的 Exec 入口（清理等维护语句）。
func exec(db *gorm.DB, ctx context.Context, sql string, args ...any) *gorm.DB {
	return db.WithContext(ctx).Exec(translateSQL(db, sql), args...)
}
