package admin

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
)

// ErrorRuleHandler manages the platform-level error_classification_rules table that
// drives service.ErrorClassifier. Writes are super-admin only (gated by RequireAction
// in route registration); the table itself is global (no org scoping).
type ErrorRuleHandler struct {
	repo     *repository.ErrorRuleRepo
	auditSvc *service.AuditService // nil in Community
}

func NewErrorRuleHandler(repo *repository.ErrorRuleRepo, auditSvc *service.AuditService) *ErrorRuleHandler {
	return &ErrorRuleHandler{repo: repo, auditSvc: auditSvc}
}

var (
	validMatchFields     = map[string]struct{}{"status": {}, "code": {}, "type": {}, "message": {}}
	validScopes          = map[string]struct{}{"account": {}, "model": {}}
	validClassifications = map[string]struct{}{"quota": {}}
)

func (h *ErrorRuleHandler) List(c *gin.Context) {
	rules, err := h.repo.List(c.Request.Context())
	if err != nil {
		internalErr(c, err, "list error rules failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rules})
}

func (h *ErrorRuleHandler) Create(c *gin.Context) {
	var input struct {
		MatchField     string  `json:"match_field" binding:"required"`
		Pattern        string  `json:"pattern" binding:"required"`
		Classification string  `json:"classification"`
		ProviderType   *string `json:"provider_type"`
		Scope          string  `json:"scope"`
		Priority       int     `json:"priority"`
		Enabled        *bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if err := validateErrorRule(input.MatchField, input.Scope, input.Classification); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	enabled := true // default enabled; the model has no gorm default for this bool
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	r := &model.ErrorClassificationRule{
		MatchField:     input.MatchField,
		Pattern:        input.Pattern,
		Classification: defaultStr(input.Classification, "quota"),
		ProviderType:   input.ProviderType,
		Scope:          defaultStr(input.Scope, "account"),
		Priority:       defaultInt(input.Priority, 100),
		Enabled:        enabled,
	}
	if err := h.repo.Create(c.Request.Context(), r); err != nil {
		internalErr(c, err, "create error rule failed")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "error_rule:create", "error_rule", fmt.Sprintf("%d", r.ID), r.Pattern, nil)
	}
	c.JSON(http.StatusCreated, gin.H{"data": r})
}

func (h *ErrorRuleHandler) Update(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	var input struct {
		MatchField     *string  `json:"match_field"`
		Pattern        *string  `json:"pattern"`
		Classification *string  `json:"classification"`
		ProviderType   *string  `json:"provider_type"`
		Scope          *string  `json:"scope"`
		Priority       *int     `json:"priority"`
		Enabled        *bool    `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	r, err := h.repo.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}

	matchField, scope, classification := r.MatchField, r.Scope, r.Classification
	if input.MatchField != nil {
		matchField = *input.MatchField
	}
	if input.Scope != nil {
		scope = *input.Scope
	}
	if input.Classification != nil {
		classification = *input.Classification
	}
	if err := validateErrorRule(matchField, scope, classification); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	if input.MatchField != nil {
		r.MatchField = *input.MatchField
	}
	if input.Pattern != nil {
		r.Pattern = *input.Pattern
	}
	if input.Classification != nil {
		r.Classification = *input.Classification
	}
	r.ProviderType = input.ProviderType // nil clears provider_type (global); pointer scopes it
	if input.Scope != nil {
		r.Scope = *input.Scope
	}
	if input.Priority != nil {
		r.Priority = *input.Priority
	}
	if input.Enabled != nil {
		r.Enabled = *input.Enabled
	}

	if err := h.repo.Update(c.Request.Context(), r); err != nil {
		internalErr(c, err, "update error rule failed")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "error_rule:update", "error_rule", fmt.Sprintf("%d", id), r.Pattern, nil)
	}
	c.JSON(http.StatusOK, gin.H{"data": r})
}

func (h *ErrorRuleHandler) Delete(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	r, err := h.repo.GetByID(c.Request.Context(), id)
	if err != nil {
		errorResp(c, http.StatusNotFound, ErrNotFound, "not found")
		return
	}
	if err := h.repo.Delete(c.Request.Context(), id); err != nil {
		internalErr(c, err, "delete error rule failed")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "error_rule:delete", "error_rule", fmt.Sprintf("%d", id), r.Pattern, nil)
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func validateErrorRule(matchField, scope, classification string) error {
	if _, ok := validMatchFields[matchField]; !ok {
		return fmt.Errorf("match_field must be one of status, code, type, message")
	}
	if scope != "" {
		if _, ok := validScopes[scope]; !ok {
			return fmt.Errorf("scope must be one of account, model")
		}
	}
	if classification != "" {
		if _, ok := validClassifications[classification]; !ok {
			return fmt.Errorf("classification must be one of quota")
		}
	}
	return nil
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func defaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
