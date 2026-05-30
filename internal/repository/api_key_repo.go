package repository

import (
	"context"
	"errors"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

var ErrKeyNotFound = errors.New("api key not found")

type APIKeyRepo struct {
	db *gorm.DB
}

func NewAPIKeyRepo(db *gorm.DB) *APIKeyRepo {
	return &APIKeyRepo{db: db}
}

func (r *APIKeyRepo) GetByHash(ctx context.Context, hash string) (*model.APIKey, error) {
	var key model.APIKey
	if err := r.db.WithContext(ctx).Where("key_hash = ? AND status = 1", hash).First(&key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, err
	}
	return &key, nil
}

func (r *APIKeyRepo) Create(ctx context.Context, key *model.APIKey) error {
	return r.db.WithContext(ctx).Create(key).Error
}

func (r *APIKeyRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&model.APIKey{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	return q
}

func (r *APIKeyRepo) List(ctx context.Context, orgID int64) ([]model.APIKey, error) {
	var keys []model.APIKey
	err := r.baseQuery(orgID).WithContext(ctx).Order("created_at DESC").Find(&keys).Error
	return keys, err
}

func (r *APIKeyRepo) ListByTeam(ctx context.Context, teamID int64) ([]model.APIKey, error) {
	var keys []model.APIKey
	err := r.db.WithContext(ctx).Where("team_id = ?", teamID).Order("created_at DESC").Find(&keys).Error
	return keys, err
}

func (r *APIKeyRepo) ListByCreatorID(ctx context.Context, userID int64) ([]model.APIKey, error) {
	var keys []model.APIKey
	err := r.db.WithContext(ctx).Where("created_by_id = ?", userID).Order("created_at DESC").Find(&keys).Error
	return keys, err
}

func (r *APIKeyRepo) Update(ctx context.Context, key *model.APIKey) error {
	return r.db.WithContext(ctx).Save(key).Error
}

func (r *APIKeyRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.APIKey{}, id).Error
}

func (r *APIKeyRepo) GetByID(ctx context.Context, orgID, id int64) (*model.APIKey, error) {
	var key model.APIKey
	if err := r.baseQuery(orgID).WithContext(ctx).First(&key, id).Error; err != nil {
		return nil, err
	}
	return &key, nil
}
