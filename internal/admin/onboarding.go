package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"log/slog"
)

// OnboardingHandler powers the first-run wizard: a stateless upstream probe
// (Probe) and an atomic provider+models+key creator (Commit) wrapped in one
// GORM transaction so partial failures leave no rows behind.
type OnboardingHandler struct {
	db              *gorm.DB
	encStore        *secret.EncryptedDBStore
	cryptoProvider  crypto.CryptoProvider
	secretResolver  *secret.SecretResolver
	registry        *provider.Registry
	resolver        *router.Resolver
	cacheSvc        *service.CacheService
	auditSvc        *service.AuditService
	onRegistryChange func(action, providerName string)
}

func NewOnboardingHandler(
	db *gorm.DB,
	encStore *secret.EncryptedDBStore,
	cp crypto.CryptoProvider,
	secretResolver *secret.SecretResolver,
	registry *provider.Registry,
	resolver *router.Resolver,
	cacheSvc *service.CacheService,
	auditSvc *service.AuditService,
) *OnboardingHandler {
	return &OnboardingHandler{
		db: db, encStore: encStore, cryptoProvider: cp,
		secretResolver: secretResolver, registry: registry,
		resolver: resolver, cacheSvc: cacheSvc, auditSvc: auditSvc,
	}
}

// SetOnRegistryChange wires the multi-instance registry-sync callback (same
// pattern as ProviderHandler.OnRegistryChange, assigned from app.go).
func (h *OnboardingHandler) SetOnRegistryChange(fn func(action, providerName string)) {
	h.onRegistryChange = fn
}

// ---- Probe: stateless upstream connectivity + model enumeration ----

type probeRequest struct {
	AdapterType string          `json:"adapter_type" binding:"required"`
	BaseURL     string          `json:"base_url"`
	APIKey      string          `json:"api_key"`
	ExtraConfig json.RawMessage `json:"extra_config"`
}

type probeModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// Probe constructs a throwaway provider (never persisted, never logged) and,
// if the adapter supports it, enumerates upstream models. Adapters without a
// models endpoint (Anthropic, Azure) fall back to a 1-token chat probe that
// only verifies connectivity.
func (h *OnboardingHandler) Probe(c *gin.Context) {
	var input probeRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.BaseURL != "" && !isValidProviderURL(input.BaseURL) {
		errorResp(c, http.StatusBadRequest, ErrProviderURLInvalid, "base_url must start with http:// or https://")
		return
	}
	if input.BaseURL != "" {
		if u, err := url.Parse(input.BaseURL); err == nil && isInternalHost(u.Hostname()) {
			errorResp(c, http.StatusBadRequest, ErrProviderURLInvalid, "base_url must not point to an internal address")
			return
		}
	}

	tmp := &model.Provider{
		Name:        "__probe__",
		AdapterType: input.AdapterType,
		BaseURL:     input.BaseURL,
		APIKey:      input.APIKey,
		ExtraConfig: datatypes.JSON(input.ExtraConfig),
	}
	prov, err := provider.CreateProvider(tmp, 15*time.Second)
	if err != nil {
		// Unsupported adapter_type or construction error → connectivity failed.
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "models_supported": false, "error": err.Error()}})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()

	if lister, ok := prov.(provider.ModelsLister); ok {
		start := time.Now()
		models, err := lister.ListUpstreamModels(ctx, input.APIKey)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "models_supported": true, "latency_ms": latency, "error": err.Error()}})
			return
		}
		out := make([]probeModel, 0, len(models))
		for _, m := range models {
			out = append(out, probeModel{ID: m.ID, OwnedBy: m.OwnedBy})
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{
			"success":          true,
			"models_supported": true,
			"latency_ms":       latency,
			"models":           out,
		}})
		return
	}

	// Degraded path: no /models endpoint. Verify connectivity with a 1-token chat.
	maxTokens := 1
	req := &domain.OpenAIRequest{
		Model:     "probe",
		MaxTokens: &maxTokens,
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}
	start := time.Now()
	_, err = prov.Chat(ctx, req, input.APIKey)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "models_supported": false, "latency_ms": latency, "error": err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"success":          true,
		"models_supported": false,
		"latency_ms":       latency,
	}})
}

// ---- Commit: atomic provider + models + key in one transaction ----

type onboardingProviderInput struct {
	Name        string          `json:"name" binding:"required"`
	DisplayName string          `json:"display_name" binding:"required"`
	AdapterType string          `json:"adapter_type" binding:"required"`
	BaseURL     string          `json:"base_url"`
	APIKey      string          `json:"api_key"`
	ExtraConfig json.RawMessage `json:"extra_config"`
}

type onboardingModelInput struct {
	ModelName       string  `json:"model_name" binding:"required"`
	ProviderModel   string  `json:"provider_model" binding:"required"`
	Weight          int     `json:"weight"`
	Priority        int     `json:"priority"`
	InputPrice      float64 `json:"input_price"`
	OutputPrice     float64 `json:"output_price"`
	Currency        string  `json:"currency"`
	RoutingStrategy string  `json:"routing_strategy"`
}

type onboardingKeyInput struct {
	Name          string   `json:"name" binding:"required,max=64"`
	AllowedModels []string `json:"allowed_models"`
	TPMLimit      int      `json:"tpm_limit"`
	RPMLimit      int      `json:"rpm_limit"`
	MaxBudget     float64  `json:"max_budget"`
	BudgetPeriod  string   `json:"budget_period"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

type onboardingRequest struct {
	Provider onboardingProviderInput `json:"provider" binding:"required"`
	Models   []onboardingModelInput  `json:"models"`
	Key      onboardingKeyInput      `json:"key" binding:"required"`
}

// Commit creates provider + models + api key + hash in a single transaction.
// Any failure rolls back all three tables. Post-transaction side effects
// (registry sync, cache flush, audit) run best-effort and never undo the data.
func (h *OnboardingHandler) Commit(c *gin.Context) {
	var input onboardingRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	// --- validate (reuse existing rules) ---
	if input.Provider.BaseURL != "" && !isValidProviderURL(input.Provider.BaseURL) {
		errorResp(c, http.StatusBadRequest, ErrProviderURLInvalid, "base_url must start with http:// or https://")
		return
	}
	if input.Provider.BaseURL != "" {
		if u, err := url.Parse(input.Provider.BaseURL); err == nil && isInternalHost(u.Hostname()) {
			errorResp(c, http.StatusBadRequest, ErrProviderURLInvalid, "base_url must not point to an internal address")
			return
		}
	}
	for i, m := range input.Models {
		if m.InputPrice < 0 || m.OutputPrice < 0 {
			errorResp(c, http.StatusBadRequest, ErrPricesNegative, fmt.Sprintf("models[%d]: prices must be non-negative", i))
			return
		}
		strategy := m.RoutingStrategy
		if strategy == "" {
			strategy = "weighted_random"
			input.Models[i].RoutingStrategy = strategy
		}
		if !router.IsValidStrategy(router.StrategyName(strategy), nil) {
			errorResp(c, http.StatusBadRequest, ErrInvalidRoutingStrategy, fmt.Sprintf("models[%d]: invalid routing strategy", i))
			return
		}
	}
	if input.Key.MaxBudget < 0 {
		errorResp(c, http.StatusBadRequest, ErrPricesNegative, "key.max_budget must be non-negative")
		return
	}
	if input.Key.ExpiresAt != nil && input.Key.ExpiresAt.Before(time.Now()) {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "key.expires_at must be in the future")
		return
	}

	orgID := GetOrgID(c)

	// --- build models in memory (provider_id filled inside tx) ---
	models := make([]*model.ProviderModel, 0, len(input.Models))
	for _, m := range input.Models {
		models = append(models, &model.ProviderModel{
			ModelName:       m.ModelName,
			ProviderModel:   m.ProviderModel,
			Weight:          m.Weight,
			Priority:        m.Priority,
			InputPrice:      m.InputPrice,
			OutputPrice:     m.OutputPrice,
			Currency:        model.ValidCurrency(m.Currency),
			RoutingStrategy: m.RoutingStrategy,
			Status:          1,
		})
	}

	// --- build provider (encrypt outside tx; pure CPU) ---
	p := &model.Provider{
		Name:        input.Provider.Name,
		DisplayName: input.Provider.DisplayName,
		AdapterType: input.Provider.AdapterType,
		BaseURL:     input.Provider.BaseURL,
		APIKey:      input.Provider.APIKey,
		ExtraConfig: datatypes.JSON(input.Provider.ExtraConfig),
		Status:      1,
	}
	if orgID != 0 {
		p.OrgID = &orgID
	}
	if err := h.encryptProvider(p); err != nil {
		internalErr(c, err, "encrypt provider failed")
		return
	}

	// --- generate key material (pure CPU, outside tx) ---
	rawKey, err := service.GenerateRawKey()
	if err != nil {
		internalErr(c, err, "generate key failed")
		return
	}
	keyHash := h.cryptoProvider.HashHex([]byte(rawKey))
	prefix := rawKey[:7]

	key := &model.APIKey{
		Name:         input.Key.Name,
		KeyHash:      keyHash,
		KeyPrefix:    prefix,
		Status:       1,
		TPMLimit:     input.Key.TPMLimit,
		RPMLimit:     input.Key.RPMLimit,
		MaxBudget:    input.Key.MaxBudget,
		BudgetPeriod: input.Key.BudgetPeriod,
		ExpiresAt:    input.Key.ExpiresAt,
	}
	if len(input.Key.AllowedModels) > 0 {
		key.AllowedModels, _ = json.Marshal(input.Key.AllowedModels)
	}
	if uid := GetUserID(c); uid > 0 {
		key.CreatedByID = &uid
	}
	if tid := GetTeamID(c); tid > 0 {
		key.TeamID = &tid
	}
	if orgID != 0 {
		key.OrgID = &orgID
	}

	hashAlgo := string(h.cryptoProvider.Algorithms().Hash)

	// --- transaction: provider → models → key → hash ---
	err = h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(p).Error; err != nil {
			return classifyCreateErr(err, "create provider")
		}
		for _, mm := range models {
			mm.ProviderID = p.ID
		}
		if len(models) > 0 {
			if err := tx.CreateInBatches(models, 100).Error; err != nil {
				return classifyCreateErr(err, "create models")
			}
		}
		if err := tx.Create(key).Error; err != nil {
			return classifyCreateErr(err, "create api key")
		}
		hashRecord := &model.APIKeyHash{
			KeyHash:   keyHash,
			KeyPrefix: prefix,
			HashAlgo:  hashAlgo,
			IsPrimary: true,
			APIKeyID:  key.ID,
		}
		if err := tx.Create(hashRecord).Error; err != nil {
			return classifyCreateErr(err, "create hash record")
		}
		return nil
	})
	if err != nil {
		if isDuplicateNameErr(err) {
			errorResp(c, http.StatusConflict, ErrConflict, "Provider name already exists, please choose another")
			return
		}
		internalErr(c, err, err.Error())
		return
	}

	// --- post-commit side effects (best effort) ---
	h.syncRegistry(p)
	if h.resolver != nil {
		h.resolver.Invalidate()
	}
	if h.cacheSvc != nil {
		h.cacheSvc.FlushAll(c.Request.Context())
	}
	if h.onRegistryChange != nil {
		h.onRegistryChange("reload", p.Name)
	}
	if input.Key.Name != "" && OnKeyCreated != nil {
		OnKeyCreated(input.Key.Name, "", rawKey, "")
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "onboarding:commit", "provider", fmt.Sprintf("%d", p.ID), p.Name, service.AuditDetail(map[string]any{
			"provider_id":    p.ID,
			"model_count":    len(models),
			"key_name":       input.Key.Name,
			"api_key_length": len(input.Provider.APIKey),
		}))
	}

	modelIDs := make([]int64, 0, len(models))
	for _, mm := range models {
		modelIDs = append(modelIDs, mm.ID)
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{
		"provider_id": p.ID,
		"model_ids":   modelIDs,
		"key":         rawKey,
		"key_prefix":  prefix,
	}})
}

// encryptProvider mirrors ProviderHandler.encryptProvider but on this handler's encStore.
func (h *OnboardingHandler) encryptProvider(p *model.Provider) error {
	if h.encStore == nil {
		return nil
	}
	if p.APIKey != "" && !secret.IsReference(p.APIKey) && !h.encStore.IsEncrypted(p.APIKey) {
		enc, err := h.encStore.Encrypt(p.APIKey)
		if err != nil {
			return fmt.Errorf("encrypt api_key: %w", err)
		}
		p.APIKey = enc
	}
	return h.encryptExtraConfig(p)
}

func (h *OnboardingHandler) encryptExtraConfig(p *model.Provider) error {
	if h.encStore == nil || len(p.ExtraConfig) == 0 {
		return nil
	}
	var config map[string]any
	if json.Unmarshal(p.ExtraConfig, &config) != nil {
		return nil
	}
	changed := false
	for k, v := range config {
		if !secret.IsSensitiveField(k) {
			continue
		}
		strVal, ok := v.(string)
		if !ok || strVal == "" || secret.IsReference(strVal) || h.encStore.IsEncrypted(strVal) {
			continue
		}
		enc, err := h.encStore.Encrypt(strVal)
		if err != nil {
			return fmt.Errorf("encrypt extra_config field %s: %w", k, err)
		}
		config[k] = enc
		changed = true
	}
	if changed {
		data, err := json.Marshal(config)
		if err != nil {
			slog.Warn("marshal extra_config failed in onboarding encrypt", "error", err)
		}
		p.ExtraConfig = data
	}
	return nil
}

// syncRegistry mirrors ProviderHandler.syncRegistry (providers.go:260).
func (h *OnboardingHandler) syncRegistry(p *model.Provider) {
	if p.Status != 1 {
		return
	}
	cp := *p
	if h.secretResolver != nil {
		resolved, err := h.secretResolver.Resolve(context.Background(), cp.APIKey)
		if err != nil {
			slog.Error("onboarding syncRegistry: failed to resolve api_key", "name", p.Name, "error", err)
			return
		}
		cp.APIKey = resolved
		if len(cp.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(cp.ExtraConfig, &config) == nil {
				if err := h.secretResolver.ResolveExtraConfigSecrets(context.Background(), config); err != nil {
					slog.Error("onboarding syncRegistry: failed to resolve extra_config secrets", "name", p.Name, "error", err)
					return
				}
				if data, err := json.Marshal(config); err == nil {
					cp.ExtraConfig = data
				}
			}
		}
	}
	prov, err := provider.NewFromModel(&cp, 300*time.Second)
	if err != nil {
		slog.Error("onboarding: failed to create provider", "name", p.Name, "adapter_type", p.AdapterType, "error", err)
		return
	}
	h.registry.Register(p.Name, prov)
	slog.Info("onboarding: synced provider to registry", "name", p.Name, "adapter_type", p.AdapterType)
}

// classifyCreateErr annotates a GORM create error with which step failed while
// preserving the original for duplicate-name detection upstream.
func classifyCreateErr(err error, step string) error {
	return fmt.Errorf("%s: %w", step, err)
}

// isDuplicateNameErr reports whether err is a unique-constraint violation
// (e.g. duplicate provider.Name). Matches Postgres, SQLite and MySQL shapes.
func isDuplicateNameErr(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "Duplicate entry") ||
		strings.Contains(msg, "UNIQUE constraint") ||
		strings.Contains(msg, "uniqueIndex")
}
