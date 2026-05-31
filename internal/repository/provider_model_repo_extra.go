package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type ProviderModelCRUDRepo struct {
	db *gorm.DB
}

func NewProviderModelCRUDRepo(db *gorm.DB) *ProviderModelCRUDRepo {
	return &ProviderModelCRUDRepo{db: db}
}

func (r *ProviderModelCRUDRepo) List(ctx context.Context, orgID int64) ([]model.ProviderModel, error) {
	var models []model.ProviderModel
	q := r.db.WithContext(ctx).Preload("Provider").Order("created_at DESC")
	if orgID != 0 {
		q = q.Joins("JOIN providers ON providers.id = provider_models.provider_id").
			Where("providers.org_id = ? OR providers.org_id IS NULL", orgID)
	}
	err := q.Find(&models).Error
	return models, err
}

func (r *ProviderModelCRUDRepo) Create(ctx context.Context, m *model.ProviderModel, orgID int64) error {
	if orgID != 0 {
		var count int64
		r.db.WithContext(ctx).Model(&model.Provider{}).
			Where("id = ? AND (org_id = ? OR org_id IS NULL)", m.ProviderID, orgID).
			Count(&count)
		if count == 0 {
			return fmt.Errorf("provider not found or not accessible")
		}
	}
	return r.db.WithContext(ctx).Create(m).Error
}

func (r *ProviderModelCRUDRepo) Update(ctx context.Context, m *model.ProviderModel, orgID int64) error {
	return r.db.WithContext(ctx).Save(m).Error
}

func (r *ProviderModelCRUDRepo) Delete(ctx context.Context, id int64, orgID int64) error {
	if orgID != 0 {
		var count int64
		r.db.WithContext(ctx).Model(&model.ProviderModel{}).
			Joins("JOIN providers ON providers.id = provider_models.provider_id").
			Where("provider_models.id = ? AND (providers.org_id = ? OR providers.org_id IS NULL)", id, orgID).
			Count(&count)
		if count == 0 {
			return errors.New("provider model not found or not accessible")
		}
	}
	return r.db.WithContext(ctx).Delete(&model.ProviderModel{}, id).Error
}

func (r *ProviderModelCRUDRepo) FirstByProviderID(ctx context.Context, providerID int64) (*model.ProviderModel, error) {
	var m model.ProviderModel
	if err := r.db.WithContext(ctx).Where("provider_id = ? AND status = 1", providerID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ProviderModelCRUDRepo) GetByID(ctx context.Context, id int64, orgID int64) (*model.ProviderModel, error) {
	var m model.ProviderModel
	q := r.db.WithContext(ctx).Preload("Provider")
	if orgID != 0 {
		q = q.Joins("JOIN providers ON providers.id = provider_models.provider_id").
			Where("providers.org_id = ? OR providers.org_id IS NULL", orgID)
	}
	if err := q.First(&m, id).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *ProviderModelCRUDRepo) CountByProviderID(ctx context.Context, providerID int64) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&model.ProviderModel{}).Where("provider_id = ?", providerID).Count(&count).Error
	return count, err
}
