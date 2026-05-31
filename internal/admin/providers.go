package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
)

type ProviderHandler struct {
	repo           ProviderRepository
	modelRepo      ProviderModelRepository
	cache          CacheInvalidator
	registry       *provider.Registry
	cacheSvc       *service.CacheService
	secretResolver *secret.SecretResolver
	encStore       *secret.EncryptedDBStore
	auditSvc       *service.AuditService
	usageSvc       *service.UsageService
	latencySvc     *service.LatencyService
	// OnRegistryChange is called after a local registry mutation (create/update/delete).
	// action is "reload" or "remove"; providerName is the affected provider.
	// When nil (single-instance Community), the call is skipped.
	OnRegistryChange func(action, providerName string)
}

func NewProviderHandler(repo ProviderRepository, modelRepo ProviderModelRepository, cache CacheInvalidator, registry *provider.Registry, cacheSvc *service.CacheService, secretResolver *secret.SecretResolver, encStore *secret.EncryptedDBStore, auditSvc *service.AuditService, usageSvc *service.UsageService, latencySvc *service.LatencyService) *ProviderHandler {
	return &ProviderHandler{repo: repo, modelRepo: modelRepo, cache: cache, registry: registry, cacheSvc: cacheSvc, secretResolver: secretResolver, encStore: encStore, auditSvc: auditSvc, usageSvc: usageSvc, latencySvc: latencySvc}
}

func (h *ProviderHandler) List(c *gin.Context) {
	orgID := GetOrgID(c)
	providers, err := h.repo.List(c.Request.Context(), orgID)
	if err != nil {
		internalErr(c, err, "list providers failed")
		return
	}
	for i := range providers {
		providers[i] = *redactExtraConfig(&providers[i])
	}

	// Enrich with latency data from Redis
	type providerWithLatency struct {
		model.Provider
		Latency float64 `json:"latency"`
	}
	result := make([]providerWithLatency, len(providers))
	for i, p := range providers {
		entry := providerWithLatency{Provider: p}
		if h.latencySvc != nil {
			entry.Latency = h.latencySvc.GetAvgLatency(c.Request.Context(), p.Name)
		}
		result[i] = entry
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *ProviderHandler) Create(c *gin.Context) {
	var input struct {
		Name        string          `json:"name" binding:"required"`
		DisplayName string          `json:"display_name" binding:"required"`
		AdapterType string          `json:"adapter_type" binding:"required"`
		BaseURL     string          `json:"base_url"`
		APIKey      string          `json:"api_key"`
		ExtraConfig json.RawMessage `json:"extra_config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	orgID := GetOrgID(c)
	if license.G().CurrentTier() == license.TierCommunity {
		providers, err := h.repo.List(c.Request.Context(), orgID)
		if err != nil {
			internalErr(c, err, "count providers failed")
			return
		}
		if len(providers) >= 3 {
			errorResp(c, http.StatusForbidden, ErrCommunityProviderLimit, "Community edition is limited to 3 providers. Upgrade to Pro for unlimited providers.")
			return
		}
	}
	if input.BaseURL != "" && !isValidProviderURL(input.BaseURL) {
		errorResp(c, http.StatusBadRequest, ErrProviderURLInvalid, "base_url must start with http:// or https://")
		return
	}

	p := &model.Provider{
		Name:        input.Name,
		DisplayName: input.DisplayName,
		AdapterType: input.AdapterType,
		BaseURL:     input.BaseURL,
		APIKey:      input.APIKey,
		ExtraConfig: datatypes.JSON(input.ExtraConfig),
		Status:      1,
	}
	if orgID != 0 {
		p.OrgID = &orgID
	}
	if err := h.encryptProvider(p); err != nil {
		internalErr(c, err, "encrypt provider failed")
		return
	}
	if err := h.repo.Create(c.Request.Context(), p); err != nil {
		internalErr(c, err, "create provider failed")
		return
	}
	h.syncRegistry(p)
	h.cache.Invalidate()
	if h.cacheSvc != nil {
		h.cacheSvc.FlushAll(c.Request.Context())
	}
	if h.OnRegistryChange != nil {
		h.OnRegistryChange("reload", p.Name)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "provider:create", "provider", fmt.Sprintf("%d", p.ID), p.Name, service.AuditDetail(map[string]any{"after": sanitizeProviderMap(input)}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": redactExtraConfig(p)})
}

func (h *ProviderHandler) Update(c *gin.Context) {
	orgID := GetOrgID(c)
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	p, err := h.repo.GetByID(c.Request.Context(), orgID, id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	before := sanitizeProviderFromModel(p)

	var input struct {
		Name        *string         `json:"name"`
		DisplayName *string         `json:"display_name"`
		AdapterType *string         `json:"adapter_type"`
		BaseURL     *string         `json:"base_url"`
		APIKey      *string         `json:"api_key"`
		Status      *int16          `json:"status"`
		ExtraConfig json.RawMessage `json:"extra_config"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	if input.Name != nil {
		p.Name = *input.Name
	}
	if input.DisplayName != nil {
		p.DisplayName = *input.DisplayName
	}
	if input.AdapterType != nil {
		p.AdapterType = *input.AdapterType
	}
	if input.BaseURL != nil {
		p.BaseURL = *input.BaseURL
	}
	if input.APIKey != nil {
		p.APIKey = *input.APIKey
		if p.APIKey != "" && h.encStore != nil && !secret.IsReference(p.APIKey) && !h.encStore.IsEncrypted(p.APIKey) {
			encrypted, err := h.encStore.Encrypt(p.APIKey)
			if err != nil {
				internalErr(c, err, "encrypt api_key failed")
				return
			}
			p.APIKey = encrypted
		}
	}
	if input.Status != nil {
		p.Status = *input.Status
	}
	if input.ExtraConfig != nil {
		p.ExtraConfig = datatypes.JSON(input.ExtraConfig)
		if err := h.encryptExtraConfig(p); err != nil {
			internalErr(c, err, "encrypt extra_config failed")
			return
		}
	}

	if err := h.repo.Update(c.Request.Context(), p); err != nil {
		internalErr(c, err, "update provider failed")
		return
	}
	h.syncRegistry(p)
	h.cache.Invalidate()
	// Only flush cache if functional fields changed
	if input.BaseURL != nil || input.APIKey != nil || input.AdapterType != nil || input.Status != nil || input.ExtraConfig != nil {
		if h.cacheSvc != nil {
			h.cacheSvc.FlushAll(c.Request.Context())
		}
	}
	if h.OnRegistryChange != nil {
		h.OnRegistryChange("reload", p.Name)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "provider:update", "provider", fmt.Sprintf("%d", id), p.Name, service.AuditDetail(map[string]any{"before": before, "after": sanitizeProviderFromModel(p)}))
	}
	c.JSON(http.StatusOK, gin.H{"data": p})
}

func (h *ProviderHandler) Delete(c *gin.Context) {
	orgID := GetOrgID(c)
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	p, err := h.repo.GetByID(c.Request.Context(), orgID, id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}

	count, _ := h.modelRepo.CountByProviderID(c.Request.Context(), p.ID)
	if count > 0 {
		errorResp(c, http.StatusConflict, ErrProviderHasModels, fmt.Sprintf("provider still has %d model mappings, delete them first", count))
		return
	}

	if err := h.repo.Delete(c.Request.Context(), id); err != nil {
		internalErr(c, err, "delete provider failed")
		return
	}
	h.registry.Remove(p.Name)
	slog.Info("unregistered provider", "name", p.Name)
	h.cache.Invalidate()
	if h.cacheSvc != nil {
		h.cacheSvc.FlushAll(c.Request.Context())
	}
	if h.OnRegistryChange != nil {
		h.OnRegistryChange("remove", p.Name)
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "provider:delete", "provider", fmt.Sprintf("%d", id), p.Name, service.AuditDetail(map[string]any{"before": sanitizeProviderFromModel(p)}))
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *ProviderHandler) syncRegistry(p *model.Provider) {
	if p.Status != 1 {
		h.registry.Remove(p.Name)
		slog.Info("unregistered provider (disabled)", "name", p.Name)
		return
	}
	// Work on a copy to avoid mutating the DB model
	cp := *p
	if h.secretResolver != nil {
		resolved, err := h.secretResolver.Resolve(context.Background(), cp.APIKey)
		if err != nil {
			slog.Error("syncRegistry: failed to resolve api_key", "name", p.Name, "error", err)
			return
		}
		cp.APIKey = resolved
		if len(cp.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(cp.ExtraConfig, &config) == nil {
				if err := h.secretResolver.ResolveExtraConfigSecrets(context.Background(), config); err != nil {
					slog.Error("syncRegistry: failed to resolve extra_config secrets", "name", p.Name, "error", err)
					return
				}
				data, err := json.Marshal(config)
				if err != nil {
					slog.Warn("marshal extra_config failed in syncRegistry", "error", err)
				}
				cp.ExtraConfig = data
			}
		}
	}
	prov, err := provider.NewFromModel(&cp, 300*time.Second)
	if err != nil {
		slog.Error("failed to create provider", "name", p.Name, "adapter_type", p.AdapterType, "error", err)
		return
	}
	h.registry.Register(p.Name, prov)
	slog.Info("synced provider to registry", "name", p.Name, "adapter_type", p.AdapterType)
}

func (h *ProviderHandler) Test(c *gin.Context) {
	orgID := GetOrgID(c)
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	p, err := h.repo.GetByID(c.Request.Context(), orgID, id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}

	// Find a real model name from this provider
	modelName := "test"
	if m, err := h.modelRepo.FirstByProviderID(c.Request.Context(), p.ID); err == nil {
		modelName = m.ProviderModel
	}

	// Resolve secrets on a copy before constructing provider (needed for Bedrock/Vertex)
	cp := *p
	apiKey := cp.APIKey
	if h.secretResolver != nil {
		resolved, err := h.secretResolver.Resolve(c.Request.Context(), cp.APIKey)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "error": "failed to resolve api_key: " + err.Error()}})
			return
		}
		apiKey = resolved
		cp.APIKey = resolved
		if len(cp.ExtraConfig) > 0 {
			var config map[string]any
			if json.Unmarshal(cp.ExtraConfig, &config) == nil {
				if err := h.secretResolver.ResolveExtraConfigSecrets(c.Request.Context(), config); err != nil {
					c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "error": "failed to resolve extra_config: " + err.Error()}})
					return
				}
				data, err := json.Marshal(config)
				if err != nil {
					slog.Warn("marshal extra_config failed in Test", "error", err)
				}
				cp.ExtraConfig = data
			}
		}
	}

	prov, err := provider.NewFromModel(&cp, 10*time.Second)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "error": "unsupported provider type: " + p.AdapterType}})
		return
	}

	maxTokens := 1
	req := &domain.OpenAIRequest{
		Model:     modelName,
		MaxTokens: &maxTokens,
		Messages:  []domain.OpenAIMessage{{Role: "user", Content: "hi"}},
	}

	start := time.Now()
	resp, err := prov.Chat(c.Request.Context(), req, apiKey)
	latency := time.Since(start).Milliseconds()

	if err != nil {
		slog.Error("provider test failed", "error", err, "provider_id", p.ID)
		if h.auditSvc != nil {
			h.auditSvc.LogFromContext(c, "provider:test", "provider", fmt.Sprintf("%d", p.ID), p.Name, service.AuditDetail(map[string]any{"success": false, "model": modelName, "latency_ms": latency}))
		}
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"success": false, "error": "connection failed", "latency_ms": latency}})
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "provider:test", "provider", fmt.Sprintf("%d", p.ID), p.Name, service.AuditDetail(map[string]any{"success": true, "model": modelName, "latency_ms": latency}))
	}
	h.logTestUsage(p, modelName, resp, latency, orgID)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"success":    true,
		"status":     200,
		"latency_ms": latency,
		"model":      resp.Model,
	}})
}

// encryptProvider encrypts plaintext api_key and extra_config sensitive fields if encryption is enabled.
func (h *ProviderHandler) encryptProvider(p *model.Provider) error {
	if h.encStore == nil {
		return nil
	}
	if p.APIKey != "" && !secret.IsReference(p.APIKey) && !h.encStore.IsEncrypted(p.APIKey) {
		encrypted, err := h.encStore.Encrypt(p.APIKey)
		if err != nil {
			return fmt.Errorf("encrypt api_key: %w", err)
		}
		p.APIKey = encrypted
	}
	return h.encryptExtraConfig(p)
}

// encryptExtraConfig encrypts sensitive fields in extra_config JSONB.
func (h *ProviderHandler) encryptExtraConfig(p *model.Provider) error {
	if h.encStore == nil {
		return nil
	}
	if len(p.ExtraConfig) == 0 {
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
		encrypted, err := h.encStore.Encrypt(strVal)
		if err != nil {
			return fmt.Errorf("encrypt extra_config field %s: %w", k, err)
		}
		config[k] = encrypted
		changed = true
	}
	if changed {
		data, err := json.Marshal(config)
		if err != nil {
			slog.Warn("marshal extra_config failed in encryptExtraConfig", "error", err)
		}
		p.ExtraConfig = data
	}
	return nil
}
func sanitizeProviderMap(input struct {
	Name        string          `json:"name" binding:"required"`
	DisplayName string          `json:"display_name" binding:"required"`
	AdapterType string          `json:"adapter_type" binding:"required"`
	BaseURL     string          `json:"base_url"`
	APIKey      string          `json:"api_key"`
	ExtraConfig json.RawMessage `json:"extra_config"`
}) map[string]any {
	m := map[string]any{"name": input.Name, "display_name": input.DisplayName, "adapter_type": input.AdapterType, "base_url": input.BaseURL}
	if input.APIKey != "" {
		m["api_key_length"] = len(input.APIKey)
	}
	return m
}

func sanitizeProviderFromModel(p *model.Provider) map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "display_name": p.DisplayName, "adapter_type": p.AdapterType, "base_url": p.BaseURL, "status": p.Status, "api_key_length": len(p.APIKey)}
}

// redactExtraConfig returns a copy of the Provider with sensitive extra_config fields masked.
func redactExtraConfig(p *model.Provider) *model.Provider {
	cp := *p
	if len(cp.ExtraConfig) == 0 {
		return &cp
	}
	var config map[string]any
	if json.Unmarshal(cp.ExtraConfig, &config) != nil {
		return &cp
	}
	for k := range config {
		if secret.IsSensitiveField(k) {
			config[k] = "***"
		}
	}
	data, err := json.Marshal(config)
	if err != nil {
		slog.Warn("marshal extra_config failed in redactExtraConfig", "error", err)
	}
	cp.ExtraConfig = data
	return &cp
}

func isValidProviderURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return s == "http" || s == "https"
}

func (h *ProviderHandler) ListAdapters(c *gin.Context) {
	adapters := provider.ListAdapters()
	c.JSON(http.StatusOK, gin.H{"data": adapters})
}

func (h *ProviderHandler) logTestUsage(p *model.Provider, modelName string, resp *domain.OpenAIResponse, latencyMs int64, orgID int64) {
	if h.usageSvc == nil {
		return
	}
	inputTokens := 0
	outputTokens := 0
	if resp.Usage.PromptTokens > 0 {
		inputTokens = resp.Usage.PromptTokens
	}
	if resp.Usage.CompletionTokens > 0 {
		outputTokens = resp.Usage.CompletionTokens
	}
	go func() {
		defer func() { recover() }()
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.usageSvc.Log(bgCtx, &service.UsageEntry{
			RouteType:      "test",
			ModelRequested: modelName,
			ModelUsed:      resp.Model,
			ProviderID:     p.ID,
			OrgID:          orgID,
			InputTokens:    inputTokens,
			OutputTokens:   outputTokens,
			LatencyMs:      latencyMs,
			StatusCode:     200,
		})
	}()
}
