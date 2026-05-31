package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ModelsHandler struct {
	db *gorm.DB
}

func NewModelsHandler(db *gorm.DB) *ModelsHandler {
	return &ModelsHandler{db: db}
}

func (h *ModelsHandler) ListModels(c *gin.Context) {
	var names []string
	query := h.db.Table("provider_models").
		Joins("JOIN providers ON providers.id = provider_models.provider_id AND providers.deleted_at IS NULL").
		Where("provider_models.status = 1 AND provider_models.deleted_at IS NULL AND providers.status = 1")
	if orgID := c.GetInt64("org_id"); orgID != 0 {
		query = query.Where("providers.org_id = ? OR providers.org_id IS NULL", orgID)
	}
	query.Distinct("model_name").
		Pluck("model_name", &names)

	models := make([]gin.H, len(names))
	for i, name := range names {
		models[i] = gin.H{
			"id":       name,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "crosslink",
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}
