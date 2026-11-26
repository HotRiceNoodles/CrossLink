package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// errorResp sends an error response with both human-readable message and machine-readable code.
func errorResp(c *gin.Context, status int, code string, msg string) {
	c.JSON(status, gin.H{"error": msg, "error_code": code})
}

// internalErr logs the error and returns a generic 500 response.
func internalErr(c *gin.Context, err error, msg string) {
	slog.Error(msg, "error", err, "path", c.Request.URL.Path)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error", "error_code": "internal_error"})
}

type CacheInvalidator interface {
	Invalidate()
}

type ModelHandler struct {
	repo     *repository.ProviderModelCRUDRepo
	cache    CacheInvalidator
	cacheSvc *service.CacheService
	auditSvc *service.AuditService
	// OnModelChange is called after a successful local commit of a model
	// mapping mutation (create/update/delete), with the affected model name.
	// Wired by the app layer to the M1 cross-instance invalidate_model
	// broadcast; nil (single-instance, no Redis) skips the call — the local
	// cache invalidation above already covers this node.
	OnModelChange func(modelName string)
}

func NewModelHandler(repo *repository.ProviderModelCRUDRepo, cache CacheInvalidator, cacheSvc *service.CacheService, auditSvc *service.AuditService) *ModelHandler {
	return &ModelHandler{repo: repo, cache: cache, cacheSvc: cacheSvc, auditSvc: auditSvc}
}

func (h *ModelHandler) List(c *gin.Context) {
	orgID := GetOrgID(c)
	models, err := h.repo.List(c.Request.Context(), orgID)
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
		Priority        *int            `json:"priority"`
		InputPrice      float64         `json:"input_price"`
		OutputPrice     float64         `json:"output_price"`
		Currency        string          `json:"currency"`
		RoutingStrategy string          `json:"routing_strategy"`
		ExtraConfig     json.RawMessage `json:"extra_config"`
		// MaxContext: the upstream model's context window (tokens). Consumed
		// by context analysis and budget estimation; absent (nil) = unknown,
		// analysis degrades to the FlagWindowUnknown path.
		MaxContext *int `json:"max_context"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.InputPrice < 0 || input.OutputPrice < 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "prices_negative"}))
		}
		errorResp(c, http.StatusBadRequest, ErrPricesNegative, "prices must be non-negative")
		return
	}
	// G4: routing fields bounds — a negative weight silently lands in the
	// fallback-only pool (strategy splits primaries at >0); priority < 1 has
	// no defined meaning (the column default is 1). Priority is a pointer so
	// an ABSENT field defaults to 1 (matching the column default and Update's
	// nil-means-untouched semantics); only an explicitly-sent value <1 is
	// rejected. The enterprise sync dialog (ProviderSyncDialog) intentionally
	// creates mappings with the three identity fields only — the int zero
	// value made those requests 400 (r81x field incident).
	if input.Weight < 0 || (input.Priority != nil && *input.Priority < 1) {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "routing_fields_invalid"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "weight must be >= 0 and priority must be >= 1")
		return
	}
	priority := 1
	if input.Priority != nil {
		priority = *input.Priority
	}
	// G1: context window, when declared, must be positive.
	if input.MaxContext != nil && *input.MaxContext <= 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "max_context must be a positive token count")
		return
	}
	// G3: duplicate live mapping (provider, model_name) skews weighted
	// routing (the same candidate appears twice). Soft-deleted rows are
	// excluded — a DB unique index cannot express that portably across the
	// three dialects, so the check is a pre-create query.
	if existing, err := h.repo.FindByProviderAndName(c.Request.Context(), input.ProviderID, input.ModelName); err == nil && existing != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "model_mapping_exists"}))
		}
		errorResp(c, http.StatusConflict, ErrModelMappingExists, fmt.Sprintf("provider %d already has a live mapping for model %q", input.ProviderID, input.ModelName))
		return
	}
	// M3.2 write half: request_overrides / allowed_extensions keys are
	// validated BEFORE the mapping row is persisted — illegal keys fail the
	// write instead of being silently ignored at dispatch.
	if err := router.ValidateModelExtensionConfig(input.ExtraConfig); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "model_extension_invalid", "error": err.Error()}))
		}
		errorResp(c, http.StatusBadRequest, ErrModelExtensionInvalid, err.Error())
		return
	}
	// supports_responses capability gate: the 3A flag on an adapter that
	// cannot serve it (anthropic protocol) would silently no-op at runtime —
	// fail the write instead (same no-silent-ignore principle).
	if !h.validateResponsesCapability(c, input.ExtraConfig, input.ProviderID, GetOrgID(c), "model:create", "", input.ModelName) {
		return
	}

	if input.RoutingStrategy == "" {
		input.RoutingStrategy = "weighted_random"
	}
	if !router.IsValidStrategy(router.StrategyName(input.RoutingStrategy), nil) {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", input.ModelName, service.AuditDetail(map[string]any{"reason": "invalid_routing_strategy"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRoutingStrategy, "invalid routing strategy")
		return
	}

	m := &model.ProviderModel{
		ProviderID:      input.ProviderID,
		ModelName:       input.ModelName,
		ProviderModel:   input.ProviderModel,
		Weight:          input.Weight,
		Priority:        priority,
		InputPrice:      input.InputPrice,
		OutputPrice:     input.OutputPrice,
		Currency:        model.ValidCurrency(input.Currency),
		RoutingStrategy: input.RoutingStrategy,
		ExtraConfig:     datatypes.JSON(input.ExtraConfig),
		MaxContext:      input.MaxContext,
		Status:          1,
	}
	orgID := GetOrgID(c)
	if err := h.repo.Create(c.Request.Context(), m, orgID); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:create", "model", "", m.ModelName, service.AuditDetail(map[string]any{"reason": "internal_error", "error": err.Error()}))
		}
		internalErr(c, err, "create model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), input.ModelName)
	h.cacheSvc.InvalidateModelCache(input.ModelName)
	if h.OnModelChange != nil {
		h.OnModelChange(input.ModelName)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:create", "model", fmt.Sprintf("%d", m.ID), m.ModelName, service.AuditDetail(map[string]any{"after": map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID}}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": m})
}

func (h *ModelHandler) Delete(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:delete", "model", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "invalid_id"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	// Fetch model name before deletion for cache invalidation
	orgID := GetOrgID(c)
	m, err := h.repo.GetByID(c.Request.Context(), id, orgID)
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:delete", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "not_found", "error": err.Error()}))
		}
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	if err := h.repo.Delete(c.Request.Context(), id, orgID); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:delete", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"reason": "internal_error", "error": err.Error()}))
		}
		internalErr(c, err, "delete model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), m.ModelName)
	h.cacheSvc.InvalidateModelCache(m.ModelName)
	if h.OnModelChange != nil {
		h.OnModelChange(m.ModelName)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:delete", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"before": map[string]any{"id": m.ID, "model_name": m.ModelName}}))
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *ModelHandler) Update(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", c.Param("id"), "", service.AuditDetail(map[string]any{"reason": "invalid_id"}))
		}
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
		// MaxContext: non-nil sets the window (0 clears it back to unknown);
		// nil leaves it unchanged.
		MaxContext *int `json:"max_context"`
		// M4/P-8: recognized only so a rename attempt gets an explicit 400
		// instead of being silently dropped by the update whitelist. The two
		// keys key the resolver/response caches — changing them in place would
		// desynchronize every cache; delete + recreate is the sanctioned path.
		ModelName     *string `json:"model_name"`
		ProviderModel *string `json:"provider_model"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.InputPrice != nil && *input.InputPrice < 0 || input.OutputPrice != nil && *input.OutputPrice < 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "prices_negative"}))
		}
		errorResp(c, http.StatusBadRequest, ErrPricesNegative, "prices must be non-negative")
		return
	}
	// G4: same bounds as Create.
	if (input.Weight != nil && *input.Weight < 0) || (input.Priority != nil && *input.Priority < 1) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "weight must be >= 0 and priority must be >= 1")
		return
	}
	// G1: 0 clears (unknown), positive sets, negative rejects.
	if input.MaxContext != nil && *input.MaxContext < 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "max_context must be a positive token count (0 clears)")
		return
	}
	if input.RoutingStrategy != nil && !router.IsValidStrategy(router.StrategyName(*input.RoutingStrategy), nil) {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "invalid_routing_strategy"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRoutingStrategy, "invalid routing strategy")
		return
	}
	// M3.2 write half (same rule as Create): extension control keys must be
	// valid before the rewrite is persisted.
	if input.ExtraConfig != nil {
		if err := router.ValidateModelExtensionConfig(input.ExtraConfig); err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "model_extension_invalid", "error": err.Error()}))
			}
			errorResp(c, http.StatusBadRequest, ErrModelExtensionInvalid, err.Error())
			return
		}
	}

	orgID := GetOrgID(c)
	m, err := h.repo.GetByID(c.Request.Context(), id, orgID)
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"reason": "not_found", "error": err.Error()}))
		}
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	before := map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID, "weight": m.Weight, "priority": m.Priority, "input_price": m.InputPrice, "output_price": m.OutputPrice, "currency": m.Currency, "status": m.Status, "routing_strategy": m.RoutingStrategy}
	// supports_responses capability gate (same rule as Create, here against
	// the loaded mapping's provider).
	if input.ExtraConfig != nil {
		if !h.validateResponsesCapability(c, input.ExtraConfig, m.ProviderID, orgID, "model:update", fmt.Sprintf("%d", id), m.ModelName) {
			return
		}
	}

	// M4/P-8: model_name and provider_model are immutable — passing a
	// DIFFERENT value is an explicit 400; passing the same value (or omitting
	// the keys) stays compatible with clients that echo the full object back.
	if (input.ModelName != nil && *input.ModelName != m.ModelName) ||
		(input.ProviderModel != nil && *input.ProviderModel != m.ProviderModel) {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"reason": "model_immutable_fields"}))
		}
		errorResp(c, http.StatusBadRequest, ErrModelImmutableFields, "model_name and provider_model are immutable; delete and recreate the mapping to change them")
		return
	}

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
	if input.MaxContext != nil {
		if *input.MaxContext == 0 {
			m.MaxContext = nil // explicit clear back to unknown
		} else {
			m.MaxContext = input.MaxContext
		}
	}

	if err := h.repo.Update(c.Request.Context(), m, orgID); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "model:update", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"reason": "internal_error", "error": err.Error()}))
		}
		internalErr(c, err, "update model failed")
		return
	}
	h.cache.Invalidate()
	h.cacheSvc.FlushByModel(c.Request.Context(), m.ModelName)
	h.cacheSvc.InvalidateModelCache(m.ModelName)
	if h.OnModelChange != nil {
		h.OnModelChange(m.ModelName)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "model:update", "model", fmt.Sprintf("%d", id), m.ModelName, service.AuditDetail(map[string]any{"before": before, "after": map[string]any{"model_name": m.ModelName, "provider_id": m.ProviderID, "weight": m.Weight, "priority": m.Priority, "input_price": m.InputPrice, "output_price": m.OutputPrice, "currency": m.Currency, "status": m.Status, "routing_strategy": m.RoutingStrategy}}))
	}
	c.JSON(http.StatusOK, gin.H{"data": m})
}

func parseID(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// validateResponsesCapability rejects a supports_responses=true mapping whose
// target provider cannot serve the 3A passthrough (anthropic-protocol
// adapters). Returns false after writing the 400 when the flag is set and the
// provider is clearly incapable; true otherwise (flag absent, provider
// capable, or provider row unavailable — the runtime double gate still
// protects the unavailable case).
func (h *ModelHandler) validateResponsesCapability(c *gin.Context, extraConfig json.RawMessage, providerID, orgID int64, action, targetID, modelName string) bool {
	var flag struct {
		SupportsResponses bool `json:"supports_responses"`
	}
	if len(extraConfig) == 0 || json.Unmarshal(extraConfig, &flag) != nil || !flag.SupportsResponses {
		return true
	}
	p, err := h.repo.GetProvider(c.Request.Context(), providerID, orgID)
	if err != nil {
		// Provider row unreachable here (should not happen — Create already
		// requires an accessible provider): let the runtime gate decide
		// rather than blocking the write on a lookup failure.
		return true
	}
	if provider.AdapterServesResponses(p.AdapterType, p.ExtraConfig) {
		return true
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFailure(c, action, "model", targetID, modelName, service.AuditDetail(map[string]any{"reason": "model_extension_invalid", "error": "supports_responses on a provider that cannot serve it"}))
	}
	errorResp(c, http.StatusBadRequest, ErrModelExtensionInvalid,
		fmt.Sprintf("provider %q (adapter %s) cannot serve /v1/responses passthrough; supports_responses is only meaningful on OpenAI-compatible upstreams", p.Name, p.AdapterType))
	return false
}
