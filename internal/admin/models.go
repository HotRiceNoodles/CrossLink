package admin

import (
	"encoding/json"
	"log/slog"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
)

// errorResp sends an error response with both human-readable message and machine-readable code.
func errorResp(c *gin.Context, status int, code string, msg string) {
	c.JSON(status, gin.H{"error": msg, "error_code": code})
}

// internalErr logs the error and returns a generic 500 response.
func internalErr(c *gin.Context, err error, msg string) {
	slog.Error(msg, "error", err, "path", c.Request.URL.Path)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
}

type CacheInvalidator interface {
	Invalidate()
}

type ModelHandler struct {
	repo     *repository.ProviderModelCRUDRepo
	cache    CacheInvalidator
	cacheSvc *service.CacheService
	auditSvc *service.AuditService
}

func NewModelHandler(repo *repository.ProviderModelCRUDRepo, cache CacheInvalidator, cacheSvc *service.CacheService, auditSvc *service.AuditService) *ModelHandler {
	return &ModelHandler{repo: repo, cache: cache, cacheSvc: cacheSvc, auditSvc: auditSvc}
}

func (h *ModelHandler) List(c *gin.Context) {
	models, err := h.repo.List(c.Request.Context())
	if err != nil {
		internalErr(c, err, "list models failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": models})
}

func (h *ModelHandler) Create(c *gin.Context) {
	var input struct {
		ProviderID      int64           `json:"provider_id" binding:"required"`
		ModelName       string          `json:"model_name" binding:"required"`
		ProviderModel   string          `json:"provider_model" binding:"required"`
		Weight          int             `json:"weight"`
		Priority        int             `json:"priority"`
		InputPrice      float64         `json:"input_price"`
		OutputPrice     float64         `json:"output_price"`
		Currency        string          `json:"currency"`
		RoutingStrategy string          `json:"routing_strategy"`
		ExtraConfig     json.RawMessage `json:"extra_config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.InputPrice < 0 || input.OutputPrice < 0 {
		errorResp(c, http.StatusBadRequest, ErrPricesNegative, "prices must be non-negative")
		return
	}

	if input.RoutingStrategy == "" {
		input.RoutingStrategy = "weighted_random"
	}
	if !router.IsValidStrategy(router.StrategyName(input.RoutingStrategy), nil) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRoutingStrategy, "invalid routing strategy")
		return
	}

	m := &model.ProviderModel{
		ProviderID:      input.ProviderID,
		ModelName:       input.ModelName,
		ProviderModel:   input.ProviderModel,
		Weight:          input.Weight,
		Priority:        input.Priority,
		InputPrice:      input.InputPrice,
		OutputPrice:     input.OutputPrice,
		Currency:        model.ValidCurrency(input.Currency),
		RoutingStrategy: input.RoutingStrategy,
		ExtraConfig:     datatypes.JSON(input.ExtraConfig),
		Status:          1,
	}
	if err := h.repo.Create(c.Request.Context(), m); err != nil {
		internalErr(c, err, "create model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), input.ModelName)
	h.cacheSvc.InvalidateModelCache(input.ModelName)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:create", "model", fmt.Sprintf("%d", m.ID), m.ModelName, service.AuditDetail(map[string]any{"after": map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID}}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": m})
}

func (h *ModelHandler) Delete(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	// Fetch model name before deletion for cache invalidation
	m, err := h.repo.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	if err := h.repo.Delete(c.Request.Context(), id); err != nil {
		internalErr(c, err, "delete model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), m.ModelName)
	h.cacheSvc.InvalidateModelCache(m.ModelName)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:delete", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"before": map[string]any{"id": m.ID, "model_name": m.ModelName}}))
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *ModelHandler) Update(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	var input struct {
		Weight          *int            `json:"weight"`
		Priority        *int            `json:"priority"`
		InputPrice      *float64        `json:"input_price"`
		OutputPrice     *float64        `json:"output_price"`
		Currency        *string         `json:"currency"`
		Status          *int16          `json:"status"`
		RoutingStrategy *string         `json:"routing_strategy"`
		ExtraConfig     json.RawMessage `json:"extra_config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.InputPrice != nil && *input.InputPrice < 0 || input.OutputPrice != nil && *input.OutputPrice < 0 {
		errorResp(c, http.StatusBadRequest, ErrPricesNegative, "prices must be non-negative")
		return
	}
	if input.RoutingStrategy != nil && !router.IsValidStrategy(router.StrategyName(*input.RoutingStrategy), nil) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRoutingStrategy, "invalid routing strategy")
		return
	}

	m, err := h.repo.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	before := map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID, "weight": m.Weight, "priority": m.Priority, "input_price": m.InputPrice, "output_price": m.OutputPrice, "currency": m.Currency, "status": m.Status, "routing_strategy": m.RoutingStrategy}

	if input.Weight != nil {
		m.Weight = *input.Weight
	}
	if input.Priority != nil {
		m.Priority = *input.Priority
	}
	if input.InputPrice != nil {
		m.InputPrice = *input.InputPrice
	}
	if input.OutputPrice != nil {
		m.OutputPrice = *input.OutputPrice
	}
	if input.Status != nil {
		m.Status = *input.Status
	}
	if input.Currency != nil {
		m.Currency = model.ValidCurrency(*input.Currency)
	}
	if input.RoutingStrategy != nil {
		m.RoutingStrategy = *input.RoutingStrategy
	}
	if input.ExtraConfig != nil {
		m.ExtraConfig = datatypes.JSON(input.ExtraConfig)
	}

	if err := h.repo.Update(c.Request.Context(), m); err != nil {
		internalErr(c, err, "update model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), m.ModelName)
	h.cacheSvc.InvalidateModelCache(m.ModelName)
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:update", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"before": before, "after": map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID, "weight": m.Weight, "priority": m.Priority, "input_price": m.InputPrice, "output_price": m.OutputPrice, "currency": m.Currency, "status": m.Status, "routing_strategy": m.RoutingStrategy}}))
	}
	c.JSON(http.StatusOK, gin.H{"data": m})
}

func parseID(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}
