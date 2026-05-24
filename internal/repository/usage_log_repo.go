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

func (r *UsageLogRepo) Create(ctx context.Context, log *model.UsageLog) error {
	return r.db.WithContext(ctx).Create(log).Error
}
