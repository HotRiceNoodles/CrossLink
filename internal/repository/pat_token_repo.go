package repository

import (
	"context"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

// ErrPatTokenNotFound is returned when no PAT matches the query.
var ErrPatTokenNotFound = gorm.ErrRecordNotFound

type PatTokenRepo struct {
	db *gorm.DB
}

func NewPatTokenRepo(db *gorm.DB) *PatTokenRepo {
	return &PatTokenRepo{db: db}
}

func (r *PatTokenRepo) FindByHash(ctx context.Context, hash string) (*model.PatToken, error) {
	var tok model.PatToken
	if err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&tok).Error; err != nil {
		return nil, err
	}
	return &tok, nil
}

func (r *PatTokenRepo) Create(ctx context.Context, t *model.PatToken) error {
	return r.db.WithContext(ctx).Create(t).Error
}

func (r *PatTokenRepo) ListByUser(ctx context.Context, userID int64) ([]model.PatToken, error) {
	var toks []model.PatToken
	err := r.db.WithContext(ctx).Where("user_id = ?", userID).Order("created_at DESC").Find(&toks).Error
	return toks, err
}

func (r *PatTokenRepo) GetByID(ctx context.Context, id int64) (*model.PatToken, error) {
	var tok model.PatToken
	if err := r.db.WithContext(ctx).First(&tok, id).Error; err != nil {
		return nil, err
	}
	return &tok, nil
}

func (r *PatTokenRepo) Revoke(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&model.PatToken{}).Where("id = ?", id).
		Updates(map[string]interface{}{"status": 0, "revoked_at": time.Now()}).Error
}

// TouchLastUsed updates last_used_at atomically, throttled to at most once per
// 60s per token (only writes when last_used_at is NULL or older than 60s).
func (r *PatTokenRepo) TouchLastUsed(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Model(&model.PatToken{}).
		Where("id = ? AND (last_used_at IS NULL OR last_used_at < ?)", id, time.Now().Add(-60*time.Second)).
		Update("last_used_at", time.Now()).Error
}
