package repository

import (
	"context"
	"errors"
	"time"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrHashNotFound = errors.New("hash not found")

type APIKeyHashRepo struct {
	db *gorm.DB
}

func NewAPIKeyHashRepo(db *gorm.DB) *APIKeyHashRepo {
	return &APIKeyHashRepo{db: db}
}

func (r *APIKeyHashRepo) GetByHash(ctx context.Context, hash string) (*model.APIKeyHash, error) {
	var h model.APIKeyHash
	err := r.db.WithContext(ctx).
		Where("key_hash = ? AND (grace_until IS NULL OR grace_until > ?)", hash, time.Now()).
		First(&h).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrHashNotFound
		}
		return nil, err
	}
	return &h, nil
}

func (r *APIKeyHashRepo) GetKeyByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	var key model.APIKey
	err := r.db.WithContext(ctx).
		Table("api_keys").
		Select("api_keys.*").
		Joins("JOIN api_key_hashes ON api_key_hashes.api_key_id = api_keys.id").
		Where("api_key_hashes.key_hash = ? AND (api_key_hashes.grace_until IS NULL OR api_key_hashes.grace_until > ?)", hash, time.Now()).
		Where("api_keys.status = 1").
		Take(&key).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrHashNotFound
		}
		return nil, err
	}
	return &key, nil
}

func (r *APIKeyHashRepo) Create(ctx context.Context, h *model.APIKeyHash) error {
	return r.db.WithContext(ctx).Create(h).Error
}

func (r *APIKeyHashRepo) ListByAPIKeyID(ctx context.Context, apiKeyID int64) ([]model.APIKeyHash, error) {
	var hashes []model.APIKeyHash
	err := r.db.WithContext(ctx).Where("api_key_id = ?", apiKeyID).Order("created_at DESC").Find(&hashes).Error
	return hashes, err
}

func (r *APIKeyHashRepo) DeleteExpired(ctx context.Context) (int64, error) {
	result := r.db.WithContext(ctx).
		Where("grace_until IS NOT NULL AND grace_until <= ?", time.Now()).
		Delete(&model.APIKeyHash{})
	return result.RowsAffected, result.Error
}

func (r *APIKeyHashRepo) DeleteByID(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.APIKeyHash{}, id).Error
}

func (r *APIKeyHashRepo) DeleteByAPIKeyID(ctx context.Context, apiKeyID int64) error {
	return r.db.WithContext(ctx).Where("api_key_id = ?", apiKeyID).Delete(&model.APIKeyHash{}).Error
}
