package repository

import (
	"context"
	"errors"

	"github.com/crosslink/internal/provider"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// FixtureRepo is the GORM implementation of provider.FixtureStore.
type FixtureRepo struct {
	db *gorm.DB
}

func NewFixtureRepo(db *gorm.DB) *FixtureRepo {
	return &FixtureRepo{db: db}
}

// Save UPSERTs a fixture by (request_hash, model) — re-recording overwrites.
func (r *FixtureRepo) Save(ctx context.Context, f *provider.Fixture) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "request_hash"}, {Name: "model"}},
		DoUpdates: clause.AssignmentColumns([]string{"response_body", "stream_chunks", "is_stream", "updated_at", "provider_name"}),
	}).Create(f).Error
}

// Lookup finds a fixture by (model, request_hash). Returns (nil, false, nil) if not found.
func (r *FixtureRepo) Lookup(ctx context.Context, model, hash string) (*provider.Fixture, bool, error) {
	var f provider.Fixture
	err := r.db.WithContext(ctx).
		Where("model = ? AND request_hash = ?", model, hash).
		First(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &f, true, nil
}
