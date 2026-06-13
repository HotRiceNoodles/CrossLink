package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// ErrorRuleRepo accesses the global error_classification_rules table.
// Platform-level config (no org scoping); writes are super-admin only.
type ErrorRuleRepo struct {
	db *gorm.DB
}

func NewErrorRuleRepo(db *gorm.DB) *ErrorRuleRepo {
	return &ErrorRuleRepo{db: db}
}

func (r *ErrorRuleRepo) List(ctx context.Context) ([]model.ErrorClassificationRule, error) {
	var rs []model.ErrorClassificationRule
	err := r.db.WithContext(ctx).Order("priority ASC, id ASC").Find(&rs).Error
	return rs, err
}

// ListEnabled returns enabled rules ordered by priority ASC then id ASC so the
// classifier's first-match yields a deterministic tie-break (priority, then specificity).
func (r *ErrorRuleRepo) ListEnabled(ctx context.Context) ([]model.ErrorClassificationRule, error) {
	var rs []model.ErrorClassificationRule
	err := r.db.WithContext(ctx).Where("enabled = ?", true).Order("priority ASC, id ASC").Find(&rs).Error
	return rs, err
}

func (r *ErrorRuleRepo) GetByID(ctx context.Context, id int64) (*model.ErrorClassificationRule, error) {
	var m model.ErrorClassificationRule
	if err := r.db.WithContext(ctx).First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ErrorRuleRepo) Create(ctx context.Context, m *model.ErrorClassificationRule) error {
	return r.db.WithContext(ctx).Create(m).Error
}

func (r *ErrorRuleRepo) Update(ctx context.Context, m *model.ErrorClassificationRule) error {
	return r.db.WithContext(ctx).Save(m).Error
}

func (r *ErrorRuleRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.ErrorClassificationRule{}, id).Error
}
