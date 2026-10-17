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

// CreateBatch inserts a WAL replay round in ONE multi-values statement.
// Per-row Create commits one transaction per row — under real load that is
// one fsync round trip per row (~70-140ms measured through the gateway),
// capping drain at ~10 rows/s; batching amortizes the commit so a 200-row
// round costs one round trip. Rows without event_id (legacy) mint their UUID
// in buildUsageLog; conflict-noop rows are durably present, so the batch is
// all-or-nothing for ack purposes.
func (r *UsageLogRepo) CreateBatch(ctx context.Context, logs []*model.UsageLog) error {
	if len(logs) == 0 {
		return nil
	}
	withIDs, withoutIDs := 0, 0
	for _, l := range logs {
		if l.EventID != nil && *l.EventID != "" {
			withIDs++
		} else {
			withoutIDs++
		}
	}
	if withoutIDs > 0 {
		// Mixed batch: GORM applies the ON CONFLICT clause per statement; run
		// the two groups separately to keep legacy rows on the plain path.
		if withIDs > 0 {
			ids := make([]*model.UsageLog, 0, withIDs)
			for _, l := range logs {
				if l.EventID != nil && *l.EventID != "" {
					ids = append(ids, l)
				}
			}
			if err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "event_id"}},
				DoNothing: true,
			}).CreateInBatches(ids, len(ids)).Error; err != nil {
				return err
			}
		}
		legacy := make([]*model.UsageLog, 0, withoutIDs)
		for _, l := range logs {
			if l.EventID == nil || *l.EventID == "" {
				legacy = append(legacy, l)
			}
		}
		return r.db.WithContext(ctx).CreateInBatches(legacy, len(legacy)).Error
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "event_id"}},
		DoNothing: true,
	}).CreateInBatches(logs, len(logs)).Error
}
