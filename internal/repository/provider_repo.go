package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type ProviderRepo struct {
	db *gorm.DB
}

func NewProviderRepo(db *gorm.DB) *ProviderRepo {
	return &ProviderRepo{db: db}
}

func (r *ProviderRepo) baseQuery(orgID int64) *gorm.DB {
	q := r.db.Model(&model.Provider{})
	if orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	return q
}

func (r *ProviderRepo) List(ctx context.Context, orgID int64) ([]model.Provider, error) {
	var providers []model.Provider
	err := r.baseQuery(orgID).WithContext(ctx).Order("created_at DESC").Find(&providers).Error
	return providers, err
}

func (r *ProviderRepo) GetByID(ctx context.Context, orgID, id int64) (*model.Provider, error) {
	var p model.Provider
	if err := r.baseQuery(orgID).WithContext(ctx).First(&p, id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *ProviderRepo) Create(ctx context.Context, p *model.Provider) error {
	return r.db.WithContext(ctx).Create(p).Error
}

func (r *ProviderRepo) Update(ctx context.Context, p *model.Provider) error {
	return r.db.WithContext(ctx).Save(p).Error
}

func (r *ProviderRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.Provider{}, id).Error
}
