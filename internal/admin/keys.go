package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
)

type KeyHandler struct {
	keySvc   KeyService
	teamRepo TeamRepository
	rdb      *redis.Client
	auditSvc *service.AuditService
}

// OnKeyCreated is set ONCE at startup by commercial edition to dispatch key emails.
// Nil in Community — no email is sent.
// NOT safe for concurrent writes — do not reassign at runtime.
var OnKeyCreated func(keyName, email, rawKey, lang string)

func NewKeyHandler(keySvc KeyService, teamRepo TeamRepository, rdb *redis.Client, auditSvc *service.AuditService) *KeyHandler {
	return &KeyHandler{keySvc: keySvc, teamRepo: teamRepo, rdb: rdb, auditSvc: auditSvc}
}

// checkKeyOwnership verifies the caller has access to the key.
// Admin sees all; non-admin must be a member of the key's team.
func (h *KeyHandler) checkKeyOwnership(c *gin.Context, key *model.APIKey) bool {
	if IsAdmin(c) {
		return true
	}
	if key.TeamID == nil {
		return key.CreatedByID != nil && *key.CreatedByID == GetUserID(c)
	}
	_, err := h.teamRepo.GetMember(c.Request.Context(), *key.TeamID, GetUserID(c))
	return err == nil
}

func (h *KeyHandler) Create(c *gin.Context) {
	var input struct {
		Name          string   `json:"name" binding:"required,max=64"`
		Email         string   `json:"email" binding:"omitempty,email"`
		Lang          string   `json:"lang"`
		AllowedModels []string `json:"allowed_models"`
		AllowedRoutes []string `json:"allowed_routes"`
		TPMLimit      int      `json:"tpm_limit"`
		RPMLimit      int      `json:"rpm_limit"`
		MaxBudget     float64  `json:"max_budget"`
		BudgetPeriod  string   `json:"budget_period"`
		MaxCalls      int      `json:"max_calls"`
		CallPeriod    string   `json:"call_period"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.BudgetPeriod != "" && !isValidPeriod(input.BudgetPeriod) {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "budget_period must be daily, weekly, or monthly")
		return
	}
	if input.MaxBudget < 0 {
		errorResp(c, http.StatusBadRequest, ErrBudgetNegative, "max_budget must be >= 0")
		return
	}
	if input.CallPeriod != "" && !isValidPeriod(input.CallPeriod) {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "call_period must be daily, weekly, or monthly")
		return
	}
	if input.MaxCalls < 0 {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "max_calls must be >= 0")
		return
	}
	if license.G().CurrentTier() == license.TierCommunity {
		var keys []model.APIKey
		var err error
		if IsAdmin(c) {
			keys, err = h.keySvc.List(c.Request.Context(), GetOrgID(c))
		} else {
			keys, err = h.keySvc.ListByTeam(c.Request.Context(), GetTeamID(c))
		}
		if err != nil {
			internalErr(c, err, "count keys failed")
			return
		}
		if len(keys) >= 5 {
			errorResp(c, http.StatusForbidden, ErrCommunityKeyLimit, "Community edition is limited to 5 API keys. Upgrade to Pro for unlimited keys.")
			return
		}
	}

	result, err := h.keySvc.Create(c.Request.Context(), &service.CreateKeyInput{
		Name:          input.Name,
		Email:         input.Email,
		AllowedModels: input.AllowedModels,
		AllowedRoutes: input.AllowedRoutes,
		TPMLimit:      input.TPMLimit,
		RPMLimit:      input.RPMLimit,
		MaxBudget:     input.MaxBudget,
		BudgetPeriod:  input.BudgetPeriod,
		MaxCalls:      input.MaxCalls,
		CallPeriod:    input.CallPeriod,
		CreatedByID:   GetUserID(c),
		TeamID:        GetTeamID(c),
		OrgID:         GetOrgID(c),
	})
	if err != nil {
		internalErr(c, err, "create key failed")
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "key:create", "key", "", input.Name, service.AuditDetail(map[string]any{"after": map[string]any{"name": input.Name, "key_prefix": result.KeyPrefix, "max_calls": input.MaxCalls, "call_period": input.CallPeriod}}))
	}

	emailSent := false
	if input.Email != "" && OnKeyCreated != nil {
		OnKeyCreated(input.Name, input.Email, result.APIKey, input.Lang)
		emailSent = true
	}

	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"key":        result.APIKey,
			"key_prefix": result.KeyPrefix,
			"email_sent": emailSent,
			"message":    "Save this key now. It will not be shown again.",
		},
	})
}

func (h *KeyHandler) List(c *gin.Context) {
	var keys []model.APIKey
	var err error
	if IsAdmin(c) {
		keys, err = h.keySvc.List(c.Request.Context(), GetOrgID(c))
	} else {
		keys, err = h.keySvc.ListByTeam(c.Request.Context(), GetTeamID(c))
	}
	if err != nil {
		internalErr(c, err, "list keys failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": keys})
}

func (h *KeyHandler) Update(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	var input struct {
		Status        *int16    `json:"status"`
		TPMLimit      *int      `json:"tpm_limit"`
		RPMLimit      *int      `json:"rpm_limit"`
		MaxBudget     *float64  `json:"max_budget"`
		BudgetPeriod  *string   `json:"budget_period"`
		MaxCalls      *int      `json:"max_calls"`
		CallPeriod    *string   `json:"call_period"`
		AllowedModels *[]string `json:"allowed_models"`
		AllowedRoutes *[]string `json:"allowed_routes"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	if input.BudgetPeriod != nil && !isValidPeriod(*input.BudgetPeriod) {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "budget_period must be daily, weekly, or monthly")
		return
	}
	if input.CallPeriod != nil && !isValidPeriod(*input.CallPeriod) {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "call_period must be daily, weekly, or monthly")
		return
	}
	if input.MaxCalls != nil && *input.MaxCalls < 0 {
		errorResp(c, http.StatusBadRequest, ErrBudgetPeriodInvalid, "max_calls must be >= 0")
		return
	}

	key, err := h.keySvc.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrKeyNotFound, "key not found")
		return
	}

	if !h.checkKeyOwnership(c, key) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "insufficient permissions")
		return
	}

	before := map[string]any{"name": key.Name, "status": key.Status, "tpm_limit": key.TPMLimit, "rpm_limit": key.RPMLimit, "max_budget": key.MaxBudget, "budget_period": key.BudgetPeriod, "allowed_models": string(key.AllowedModels), "allowed_routes": string(key.AllowedRoutes), "max_calls": key.MaxCalls, "call_period": key.CallPeriod}

	if input.Status != nil {
		key.Status = *input.Status
	}
	if input.TPMLimit != nil {
		key.TPMLimit = *input.TPMLimit
	}
	if input.RPMLimit != nil {
		key.RPMLimit = *input.RPMLimit
	}
	if input.MaxBudget != nil {
		key.MaxBudget = *input.MaxBudget
	}
	if input.BudgetPeriod != nil {
		key.BudgetPeriod = *input.BudgetPeriod
	}
	if input.MaxCalls != nil {
		key.MaxCalls = *input.MaxCalls
	}
	if input.CallPeriod != nil {
		key.CallPeriod = *input.CallPeriod
	}
	if input.AllowedModels != nil {
		data, err := json.Marshal(*input.AllowedModels)
		if err != nil {
			slog.Warn("marshal allowed models failed", "error", err)
		}
		key.AllowedModels = data
	}
	if input.AllowedRoutes != nil {
		data, err := json.Marshal(*input.AllowedRoutes)
		if err != nil {
			slog.Warn("marshal allowed routes failed", "error", err)
		}
		key.AllowedRoutes = data
	}

	if err := h.keySvc.Update(c.Request.Context(), key); err != nil {
		internalErr(c, err, "update key failed")
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "key:update", "key", fmt.Sprintf("%d", key.ID), key.Name, service.AuditDetail(map[string]any{"before": before, "after": map[string]any{"name": key.Name, "status": key.Status, "tpm_limit": key.TPMLimit, "rpm_limit": key.RPMLimit, "max_budget": key.MaxBudget, "budget_period": key.BudgetPeriod, "allowed_models": string(key.AllowedModels), "allowed_routes": string(key.AllowedRoutes), "max_calls": key.MaxCalls, "call_period": key.CallPeriod}}))
	}
	c.JSON(http.StatusOK, gin.H{"data": key})
}

func (h *KeyHandler) Delete(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	key, err := h.keySvc.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrKeyNotFound, "key not found")
		return
	}

	if !h.checkKeyOwnership(c, key) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "insufficient permissions")
		return
	}

	// Clear Redis budget and call counters for this key
	if h.rdb != nil {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, pattern := range []string{
			fmt.Sprintf("budget:key:%d:*", id),
			fmt.Sprintf("calls:key:%d:*", id),
		} {
			var cursor uint64
			for {
				keys, next, err := h.rdb.Scan(bgCtx, cursor, pattern, 100).Result()
				if err != nil {
					break
				}
				for _, k := range keys {
					h.rdb.Del(bgCtx, k)
				}
				cursor = next
				if cursor == 0 {
					break
				}
			}
		}
		cancel()
	}

	if err := h.keySvc.Delete(c.Request.Context(), id); err != nil {
		internalErr(c, err, "delete key failed")
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "key:delete", "key", fmt.Sprintf("%d", id), key.Name, service.AuditDetail(map[string]any{"before": map[string]any{"name": key.Name}}))
	}
	c.JSON(http.StatusOK, gin.H{"data": "deleted"})
}

func (h *KeyHandler) Regenerate(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	key, err := h.keySvc.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrKeyNotFound, "key not found")
		return
	}

	if !h.checkKeyOwnership(c, key) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "insufficient permissions")
		return
	}

	result, err := h.keySvc.Regenerate(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, repository.ErrKeyNotFound) || errors.Is(err, repository.ErrHashNotFound) {
			errorResp(c, http.StatusNotFound, ErrKeyNotFound, "key not found")
			return
		}
		slog.Error("regenerate key failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "key:regenerate", "key", fmt.Sprintf("%d", id), key.Name, service.AuditDetail(map[string]any{"key_name": key.Name, "key_prefix": result.KeyPrefix}))
	}

	emailSent := false
	if key.Email != nil && *key.Email != "" && OnKeyCreated != nil {
		OnKeyCreated(key.Name, *key.Email, result.APIKey, "")
		emailSent = true
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"key":        result.APIKey,
			"key_prefix": result.KeyPrefix,
			"email_sent": emailSent,
			"message":    "Save this key now. It will not be shown again.",
		},
	})
}

func isValidPeriod(p string) bool {
	return p == "daily" || p == "weekly" || p == "monthly"
}
