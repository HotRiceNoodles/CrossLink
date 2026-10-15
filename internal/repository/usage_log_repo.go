package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	if log.EventID != nil && *log.EventID != "" {
		// Idempotent billing insert (F34): a replay of an already-committed
		// event collides on the unique event_id index. DoNothing makes the
		// duplicate a silent no-op (no error, RowsAffected 0) — the row is
		// durably present either way, which is exactly the ack semantics the
		// spool replay needs. This row is the ONLY accounting side effect of
		// a usage event; budget figures are derived from usage_logs, so
		// suppressing the duplicate suppresses the whole side effect.
		return r.db.WithContext(ctx).Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "event_id"}},
			DoNothing: true,
		}).Create(log).Error
	}
	return r.db.WithContext(ctx).Create(log).Error
}
