package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var (
	ErrReportNotFound   = errors.New("report not found")
	ErrScheduleNotFound = errors.New("schedule not found")
	ErrVersionConflict  = errors.New("report version conflict")
)

type DataLensRepository struct {
	db *gorm.DB
}

func NewDataLensRepository(db *gorm.DB) *DataLensRepository {
	return &DataLensRepository{db: db}
}

// --- Report methods ---

// ListReports returns paginated reports for an org/user.
func (r *DataLensRepository) ListReports(ctx context.Context, orgID int64, userID int64, scope string, page, pageSize int) ([]model.DataLensReport, int64, error) {
	q := r.db.WithContext(ctx).Model(&model.DataLensReport{}).Where("org_id = ?", orgID)
	if scope == "private" {
		q = q.Where("user_id = ?", userID)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var reports []model.DataLensReport
	err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&reports).Error
	return reports, total, err
}

func (r *DataLensRepository) GetReport(ctx context.Context, id int64) (*model.DataLensReport, error) {
	var report model.DataLensReport
	if err := r.db.WithContext(ctx).First(&report, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrReportNotFound
		}
		return nil, err
	}
	return &report, nil
}

func (r *DataLensRepository) CreateReport(ctx context.Context, report *model.DataLensReport) error {
	return r.db.WithContext(ctx).Create(report).Error
}

func (r *DataLensRepository) UpdateReport(ctx context.Context, report *model.DataLensReport, expectedVersion int) error {
	result := r.db.WithContext(ctx).
		Model(&model.DataLensReport{}).
		Where("id = ? AND version = ?", report.ID, expectedVersion).
		Updates(map[string]interface{}{
			"name":        report.Name,
			"description": report.Description,
			"type":        report.Type,
			"template_id": report.TemplateID,
			"scope":       report.Scope,
			"config":      report.Config,
			"is_pinned":   report.IsPinned,
			"version":     expectedVersion + 1,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrVersionConflict
	}
	return nil
}

func (r *DataLensRepository) DeleteReport(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.DataLensReport{}, id).Error
}

func (r *DataLensRepository) DuplicateReport(ctx context.Context, id int64, newName string, userID int64) (*model.DataLensReport, error) {
	src, err := r.GetReport(ctx, id)
	if err != nil {
		return nil, err
	}
	dup := &model.DataLensReport{
		OrgID:       src.OrgID,
		UserID:      userID,
		Name:        newName,
		Description: src.Description,
		Type:        src.Type,
		TemplateID:  src.TemplateID,
		Scope:       src.Scope,
		Config:      src.Config,
	}
	if err := r.CreateReport(ctx, dup); err != nil {
		return nil, fmt.Errorf("duplicate report: %w", err)
	}
	return dup, nil
}

func (r *DataLensRepository) CountReportsByUser(ctx context.Context, orgID, userID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.DataLensReport{}).
		Where("org_id = ? AND user_id = ?", orgID, userID).
		Count(&count).Error
	return count, err
}

// --- Schedule methods ---

func (r *DataLensRepository) ListSchedules(ctx context.Context, orgID int64, page, pageSize int) ([]model.DataLensSchedule, int64, error) {
	q := r.db.WithContext(ctx).Model(&model.DataLensSchedule{}).Where("org_id = ?", orgID)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var schedules []model.DataLensSchedule
	err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&schedules).Error
	return schedules, total, err
}

func (r *DataLensRepository) GetSchedule(ctx context.Context, id int64) (*model.DataLensSchedule, error) {
	var sched model.DataLensSchedule
	if err := r.db.WithContext(ctx).First(&sched, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrScheduleNotFound
		}
		return nil, err
	}
	return &sched, nil
}

func (r *DataLensRepository) CreateSchedule(ctx context.Context, sched *model.DataLensSchedule) error {
	return r.db.WithContext(ctx).Create(sched).Error
}

func (r *DataLensRepository) UpdateSchedule(ctx context.Context, sched *model.DataLensSchedule) error {
	return r.db.WithContext(ctx).Save(sched).Error
}

func (r *DataLensRepository) DeleteSchedule(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.DataLensSchedule{}, id).Error
}

// ListPendingSchedules returns schedules due for execution before the given time.
func (r *DataLensRepository) ListPendingSchedules(ctx context.Context, before time.Time) ([]model.DataLensSchedule, error) {
	var schedules []model.DataLensSchedule
	err := r.db.WithContext(ctx).
		Where("enabled = ? AND next_run_at <= ? AND deleted_at IS NULL", true, before).
		Find(&schedules).Error
	return schedules, err
}
