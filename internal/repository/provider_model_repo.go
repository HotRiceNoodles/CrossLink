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

func (r *ProviderModelRepo) FindByModelName(ctx context.Context, modelName string, orgID int64) ([]model.ProviderModel, error) {
	var models []model.ProviderModel
	q := r.db.WithContext(ctx).
		Joins("Provider").
		Where("provider_models.model_name = ? AND provider_models.status = 1", modelName)
	if orgID != 0 {
		q = q.Where("\"Provider\".org_id = ? OR \"Provider\".org_id IS NULL", orgID)
	}
	err := q.Order("provider_models.weight DESC, provider_models.priority ASC").
		Find(&models).Error
	if err != nil {
		return nil, err
	}
	return models, nil
}
