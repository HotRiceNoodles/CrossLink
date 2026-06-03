package repository

import (
	"context"
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
	ResourceType string
	UserID       int64
	OrgID        int64
	Status       string
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
	if f.ResourceType != "" {
		query = query.Where("resource_type = ?", f.ResourceType)
	}
	if f.UserID > 0 {
		query = query.Where("user_id = ?", f.UserID)
	}
	if f.OrgID != 0 {
		query = query.Where("org_id = ?", f.OrgID)
	}
	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}
	if f.Q != "" {
		escapedQ := strings.ReplaceAll(f.Q, "%", "\\%")
		escapedQ = strings.ReplaceAll(escapedQ, "_", "\\_")
		like := "%" + escapedQ + "%"
		query = query.Where(dia.ILike("resource_name", "?")+" OR "+dia.ILike("username", "?"), like, like)
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
	var total int64
	for {
		result := r.db.WithContext(ctx).
			Where("id IN (SELECT id FROM audit_logs WHERE created_at < ? LIMIT 5000)", before).
			Delete(&model.AuditLog{})
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
