package repository

import (
	"context"

	"github.com/crosslink/internal/model"
	"gorm.io/gorm"
)

type ProviderModelRepo struct {
	db *gorm.DB
}

func NewProviderModelRepo(db *gorm.DB) *ProviderModelRepo {
	return &ProviderModelRepo{db: db}
}

func (r *ProviderModelRepo) FindByModelName(ctx context.Context, modelName string) ([]model.ProviderModel, error) {
	var models []model.ProviderModel
	err := r.db.WithContext(ctx).
		Joins("Provider").
		Where("provider_models.model_name = ? AND provider_models.status = 1", modelName).
		Order("provider_models.weight DESC, provider_models.priority ASC").
		Find(&models).Error
	if err != nil {
		return nil, err
	}
	return models, nil
}
