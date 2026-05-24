package repository

import (
	"context"
	"errors"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrAlertNotFound = errors.New("alert not found")

type BudgetAlertRepo struct {
	db *gorm.DB
}

func NewBudgetAlertRepo(db *gorm.DB) *BudgetAlertRepo {
	return &BudgetAlertRepo{db: db}
}

func (r *BudgetAlertRepo) Create(ctx context.Context, alert *model.BudgetAlert) error {
	return r.db.WithContext(ctx).Create(alert).Error
}

func (r *BudgetAlertRepo) ListByTarget(ctx context.Context, scope string, targetID int64) ([]model.BudgetAlert, error) {
	var alerts []model.BudgetAlert
	q := r.db.WithContext(ctx)
	if scope == "team" {
		q = q.Where("team_id = ?", targetID)
	} else {
		q = q.Where("key_id = ?", targetID)
	}
	err := q.Find(&alerts).Error
	return alerts, err
}

func (r *BudgetAlertRepo) List(ctx context.Context) ([]model.BudgetAlert, error) {
	var alerts []model.BudgetAlert
	err := r.db.WithContext(ctx).Order("id DESC").Find(&alerts).Error
	return alerts, err
}

func (r *BudgetAlertRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.BudgetAlert{}, id).Error
}

func (r *BudgetAlertRepo) MarkTriggered(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&model.BudgetAlert{}).Where("id = ?", id).
		Update("last_triggered_at", time.Now()).Error
}

func (r *BudgetAlertRepo) GetByID(ctx context.Context, id int64) (*model.BudgetAlert, error) {
	var alert model.BudgetAlert
	if err := r.db.WithContext(ctx).First(&alert, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAlertNotFound
		}
		return nil, err
	}
	return &alert, nil
}
