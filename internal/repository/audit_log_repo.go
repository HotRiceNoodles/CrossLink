package repository

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/crosslink/internal/dialect"
	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type AuditLogRepo struct {
	db  *gorm.DB
	dia dialect.Dialect
}

func NewAuditLogRepo(db *gorm.DB, dia dialect.Dialect) *AuditLogRepo {
	return &AuditLogRepo{db: db, dia: dia}
}

func (r *AuditLogRepo) CreateBatch(ctx context.Context, logs []*model.AuditLog) error {
	return r.db.WithContext(ctx).Create(&logs).Error
}

type AuditFilter struct {
	Action       string
	Actions      []string /* action IN 集合（分类/风险/交集由前端解析后传入） */
	ResourceType string
	UserID       int64
	OrgID        int64  /* 租户上下文自动注入（≠0 等值） */
	OrgPlatform  bool   /* true → org_id IS NULL（平台级记录；优先于 OrgID） */
	Status       string /* "failure" 归一为 IN ('failure','failed','error')；其余精确等值 */
	Username     string /* 前缀匹配 */
	IPAddress    string /* 等值 */
	ExcludePAT   bool   /* true → 排除 pat:request（stats 口径：操作人=管理面操作者） */
	Q            string
	StartDate    string
	EndDate      string
	Page         int
	PageSize     int
}

func (f *AuditFilter) apply(query *gorm.DB, dia dialect.Dialect) *gorm.DB {
	if f.Action != "" {
		query = query.Where("action = ?", f.Action)
	}
	if len(f.Actions) > 0 {
		query = query.Where("action IN ?", f.Actions)
	}
	if f.ResourceType != "" {
		query = query.Where("resource_type = ?", f.ResourceType)
	}
	if f.UserID > 0 {
		query = query.Where("user_id = ?", f.UserID)
	}
	if f.OrgPlatform {
		query = query.Where("org_id IS NULL")
	} else if f.OrgID != 0 {
		query = query.Where("org_id = ?", f.OrgID)
	}
	if f.Status != "" {
		/* 失败口径归一（与前端 FAIL_STATUSES 三值同集）：历史脏值 failed/error 与 failure 同语义 */
		if f.Status == "failure" {
			query = query.Where("status IN ?", []string{"failure", "failed", "error"})
		} else {
			query = query.Where("status = ?", f.Status)
		}
	}
	if f.Username != "" {
		query = query.Where(dia.ILike("username", "?"), f.Username+"%")
	}
	if f.IPAddress != "" {
		query = query.Where("ip_address = ?", f.IPAddress)
	}
	if f.ExcludePAT {
		query = query.Where("action != ?", "pat:request")
	}
	if f.Q != "" {
		escapedQ := strings.ReplaceAll(f.Q, "%", "\\%")
		escapedQ = strings.ReplaceAll(escapedQ, "_", "\\_")
		like := "%" + escapedQ + "%"
		/* q 可解析为整数时同时精确命中事件 id（解析失败用 -1，必不命中） */
		idOrMinusOne := int64(-1)
		if n, err := strconv.ParseInt(strings.TrimSpace(f.Q), 10, 64); err == nil {
			idOrMinusOne = n
		}
		query = query.Where(
			dia.ILike("resource_name", "?")+" OR "+dia.ILike("username", "?")+" OR "+
				dia.ILike("action", "?")+" OR "+dia.ILike("resource_id", "?")+" OR id = ?",
			like, like, like, like, idOrMinusOne)
	}
	if f.StartDate != "" {
		if t, err := time.Parse("2006-01-02", f.StartDate); err == nil {
			query = query.Where("created_at >= ?", t)
		}
	}
	if f.EndDate != "" {
		if t, err := time.Parse("2006-01-02", f.EndDate); err == nil {
			query = query.Where("created_at < ?", t.AddDate(0, 0, 1))
		}
	}
	return query
}

func (r *AuditLogRepo) List(ctx context.Context, f AuditFilter) ([]model.AuditLog, int64, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 20
	}

	base := r.db.WithContext(ctx).Model(&model.AuditLog{})
	filtered := f.apply(base, r.dia)

	var total int64
	if err := filtered.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []model.AuditLog
	offset := (f.Page - 1) * f.PageSize
	if err := filtered.Order("created_at DESC").Offset(offset).Limit(f.PageSize).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

type AuditActionStatusCount struct {
	Action string `json:"action"`
	Status string `json:"status"`
	Count  int64  `json:"count"`
}

type AuditOperatorCount struct {
	Username string `json:"username"`
	Count    int64  `json:"count"`
}

type AuditStatsResult struct {
	Total       int64                    `json:"-"`
	Success     int64                    `json:"-"`
	Failure     int64                    `json:"-"`
	ActiveUsers int64                    `json:"-"`
	ByAction    []AuditActionStatusCount `json:"by_action"`
	Operators   []AuditOperatorCount     `json:"operators"`
}

/* Stats：审计中心概览聚合（2 条查询封顶）。
   风险/分类计数由前端拿 ByAction 按映射求和——后端不感知风险语义；
   失败 = status IN 三值（与 apply 归一同口径）；active_users = operators 行数
   （LIMIT 100 截断，超限由前端标注） */
func (r *AuditLogRepo) Stats(ctx context.Context, f AuditFilter) (*AuditStatsResult, error) {
	base := func() *gorm.DB { return f.apply(r.db.WithContext(ctx).Model(&model.AuditLog{}), r.dia) }

	var byAction []AuditActionStatusCount
	if err := base().Select("action, status, COUNT(*) AS count").Group("action, status").Scan(&byAction).Error; err != nil {
		return nil, err
	}
	var operators []AuditOperatorCount
	/* 操作人名单与 active_users 剔除 auth:login_failed：该行 username 是「被尝试的
	   用户名」（多为不存在的账号，审计按设计如实记录尝试值），留在名单里会以假用户名
	   污染操作人下拉并虚增活跃操作人员；按用户名追查登录尝试仍走 username 筛选/搜索 */
	if err := base().Where("action <> ?", "auth:login_failed").
		Select("username, COUNT(*) AS count").Group("username").Order("count DESC").Limit(100).Scan(&operators).Error; err != nil {
		return nil, err
	}

	st := &AuditStatsResult{ByAction: byAction, Operators: operators}
	for _, x := range byAction {
		st.Total += x.Count
		switch x.Status {
		case "success":
			st.Success += x.Count
		case "failure", "failed", "error":
			st.Failure += x.Count
		}
	}
	st.ActiveUsers = int64(len(operators))
	return st, nil
}

// StreamAll calls fn for each matching row using a database cursor.
func (r *AuditLogRepo) StreamAll(ctx context.Context, f AuditFilter, fn func(model.AuditLog) error) (int64, error) {
	base := r.db.WithContext(ctx).Model(&model.AuditLog{})
	filtered := f.apply(base, r.dia)

	var total int64
	if err := filtered.Count(&total).Error; err != nil {
		return 0, err
	}

	rows, err := filtered.Order("created_at DESC").Limit(10000).Rows()
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var log model.AuditLog
		if err := r.db.ScanRows(rows, &log); err != nil {
			return total, err
		}
		if err := fn(log); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (r *AuditLogRepo) DeleteBefore(ctx context.Context, before time.Time) (int64, error) {
	// Bounded batches only. MySQL cannot reference the delete target in its
	// own subquery (ERROR 1093), so it takes the native single-statement
	// DELETE ... LIMIT; PG/SQLite keep the id-subquery shape.
	const batchSize = 5000
	var total int64
	for {
		var result *gorm.DB
		if r.db.Dialector.Name() == "mysql" {
			result = r.db.WithContext(ctx).Exec(
				"DELETE FROM audit_logs WHERE created_at < ? LIMIT ?",
				before, batchSize)
		} else {
			result = r.db.WithContext(ctx).
				Where("id IN (SELECT id FROM audit_logs WHERE created_at < ? LIMIT ?)", before, batchSize).
				Delete(&model.AuditLog{})
		}
		if result.Error != nil {
			return total, result.Error
		}
		if result.RowsAffected == 0 {
			break
		}
		total += result.RowsAffected
	}
	return total, nil
}
