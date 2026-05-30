package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type UsageLogRepo struct {
	db *gorm.DB
}

func NewUsageLogRepo(db *gorm.DB) *UsageLogRepo {
	return &UsageLogRepo{db: db}
}

func (r *UsageLogRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&model.UsageLog{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	return q
}

func (r *UsageLogRepo) Create(ctx context.Context, log *model.UsageLog) error {
	return r.db.WithContext(ctx).Create(log).Error
}
