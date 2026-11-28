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
	var rows []struct {
		ModelName string
		Modality  string
	}
	query := h.db.Table("provider_models").
		Joins("JOIN providers ON providers.id = provider_models.provider_id AND providers.deleted_at IS NULL").
		Where("provider_models.status = 1 AND provider_models.deleted_at IS NULL AND providers.status = 1")
	if orgID := c.GetInt64("org_id"); orgID != 0 {
		query = query.Where("providers.org_id = ? OR providers.org_id IS NULL", orgID)
	}
	query.Distinct("model_name, modality").
		Select("model_name, modality").
		Scan(&rows)

	// Fold per name: a single distinct explicit modality surfaces; mixed or
	// none stays "" (unclassified — mixed same-name mappings are a
	// misconfiguration the fold deliberately does not paper over).
	modalityByName := map[string]string{}
	names := make([]string, 0, len(rows))
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.ModelName] {
			seen[r.ModelName] = true
			names = append(names, r.ModelName)
			modalityByName[r.ModelName] = r.Modality
		} else if modalityByName[r.ModelName] != r.Modality {
			modalityByName[r.ModelName] = "\x00mixed"
		}
	}

	models := make([]gin.H, len(names))
	for i, name := range names {
		modality := modalityByName[name]
		if modality == "\x00mixed" {
			modality = ""
		}
		models[i] = gin.H{
			"id":       name,
			"object":   "model",
			"created":  time.Now().Unix(),
			"owned_by": "crosslink",
			// P3: endpoint family hint for client-side filtering; "" =
			// unclassified. Additive — existing clients ignore it.
			"modality": modality,
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   models,
	})
}
