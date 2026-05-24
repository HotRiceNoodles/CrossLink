package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type ProviderModelCRUDRepo struct {
	db *gorm.DB
}

func NewProviderModelCRUDRepo(db *gorm.DB) *ProviderModelCRUDRepo {
	return &ProviderModelCRUDRepo{db: db}
}

func (r *ProviderModelCRUDRepo) List(ctx context.Context) ([]model.ProviderModel, error) {
	var models []model.ProviderModel
	err := r.db.WithContext(ctx).Preload("Provider").Order("created_at DESC").Find(&models).Error
	return models, err
}

func (r *ProviderModelCRUDRepo) Create(ctx context.Context, m *model.ProviderModel) error {
	return r.db.WithContext(ctx).Create(m).Error
}

func (r *ProviderModelCRUDRepo) Update(ctx context.Context, m *model.ProviderModel) error {
	return r.db.WithContext(ctx).Save(m).Error
}

func (r *ProviderModelCRUDRepo) Delete(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).Delete(&model.ProviderModel{}, id).Error
}

func (r *ProviderModelCRUDRepo) FirstByProviderID(ctx context.Context, providerID int64) (*model.ProviderModel, error) {
	var m model.ProviderModel
	if err := r.db.WithContext(ctx).Where("provider_id = ? AND status = 1", providerID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ProviderModelCRUDRepo) GetByID(ctx context.Context, id int64) (*model.ProviderModel, error) {
	var m model.ProviderModel
	if err := r.db.WithContext(ctx).Preload("Provider").First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ProviderModelCRUDRepo) CountByProviderID(ctx context.Context, providerID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.ProviderModel{}).Where("provider_id = ?", providerID).Count(&count).Error
	return count, err
}
