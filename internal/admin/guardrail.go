package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	validDirections = map[string]bool{"request": true, "response": true, "both": true}
	validSeverities = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}
	validActions    = map[string]bool{"block": true, "log": true, "mask": true}
)

type GuardrailHandler struct {
	db       *gorm.DB
	svc      *guardrail.GuardrailService
	auditSvc *service.AuditService
}

func NewGuardrailHandler(db *gorm.DB, svc *guardrail.GuardrailService, auditSvc *service.AuditService) *GuardrailHandler {
	return &GuardrailHandler{db: db, svc: svc, auditSvc: auditSvc}
}

func (h *GuardrailHandler) List(c *gin.Context) {
	var rules []guardrail.GuardrailRule
	query := h.db.WithContext(c.Request.Context()).Order("id ASC")

	if orgID := GetOrgID(c); orgID != 0 {
		query = query.Where("org_id = ?", orgID)
	}
	if ruleType := c.Query("type"); ruleType != "" {
		query = query.Where("type = ?", ruleType)
	}
	if enabled := c.Query("enabled"); enabled != "" {
		query = query.Where("enabled = ?", enabled == "true")
	}

	if err := query.Find(&rules).Error; err != nil {
		internalErr(c, err, "list guardrail rules failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": sanitizeRules(rules)})
}

func (h *GuardrailHandler) Create(c *gin.Context) {
	var input struct {
		Name        string          `json:"name" binding:"required"`
		Type        string          `json:"type" binding:"required"`
		Direction   string          `json:"direction" binding:"required"`
		Enabled     *bool           `json:"enabled"`
		Config      json.RawMessage `json:"config" binding:"required"`
		Severity    string          `json:"severity"`
		Action      string          `json:"action"`
		ModelFilter *string         `json:"model_filter"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	validTypes := guardrail.RegisteredTypes()
	typeValid := false
	for _, t := range validTypes {
		if t == input.Type {
			typeValid = true
			break
		}
	}
	if !typeValid {
		errorResp(c, http.StatusBadRequest, ErrInvalidGuardrailType, "invalid guardrail type")
		return
	}

	if _, err := guardrail.CreateEngine(input.Type, input.Config); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidConfig, "invalid config")
		return
	}

	if !validDirections[input.Direction] {
		errorResp(c, http.StatusBadRequest, ErrInvalidDirection, "invalid direction, must be one of: request, response, both")
		return
	}

	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	severity := "medium"
	if input.Severity != "" {
		if !validSeverities[input.Severity] {
			errorResp(c, http.StatusBadRequest, ErrInvalidSeverity, "invalid severity, must be one of: low, medium, high, critical")
			return
		}
		severity = input.Severity
	}
	action := "block"
	if input.Action != "" {
		if !validActions[input.Action] {
			errorResp(c, http.StatusBadRequest, ErrInvalidAction, "invalid action, must be one of: block, log, mask")
			return
		}
		action = input.Action
	}

	rule := &guardrail.GuardrailRule{
		Name:        input.Name,
		Type:        input.Type,
		Direction:   input.Direction,
		Enabled:     enabled,
		Config:      datatypes.JSON(input.Config),
		Severity:    severity,
		Action:      action,
		ModelFilter: input.ModelFilter,
	}
	if orgID := GetOrgID(c); orgID != 0 {
		rule.OrgID = &orgID
	}
	if err := h.db.WithContext(c.Request.Context()).Create(rule).Error; err != nil {
		internalErr(c, err, "create guardrail rule failed")
		return
	}

	h.svc.InvalidateCache()
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:create", "guardrail", fmt.Sprintf("%d", rule.ID), rule.Name, service.AuditDetail(map[string]any{"after": map[string]any{"name": rule.Name, "type": rule.Type}}))
	}
	c.JSON(http.StatusCreated, gin.H{"data": sanitizeRule(*rule)})
}

func (h *GuardrailHandler) Update(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	var rule guardrail.GuardrailRule
	q := h.db.WithContext(c.Request.Context())
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&rule, id).Error; err != nil {
		errorResp(c, http.StatusNotFound, ErrRuleNotFound, "rule not found")
		return
	}

	before := map[string]any{"name": rule.Name, "type": rule.Type, "direction": rule.Direction, "enabled": rule.Enabled, "severity": rule.Severity, "action": rule.Action}

	var input struct {
		Name        *string         `json:"name"`
		Type        *string         `json:"type"`
		Direction   *string         `json:"direction"`
		Enabled     *bool           `json:"enabled"`
		Config      json.RawMessage `json:"config"`
		Severity    *string         `json:"severity"`
		Action      *string         `json:"action"`
		ModelFilter *string         `json:"model_filter"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	if input.Name != nil {
		rule.Name = *input.Name
	}
	if input.Type != nil {
		typeValid := false
		for _, t := range guardrail.RegisteredTypes() {
			if t == *input.Type {
				typeValid = true
				break
			}
		}
		if !typeValid {
			errorResp(c, http.StatusBadRequest, ErrInvalidGuardrailType, "invalid guardrail type")
			return
		}
		// Validate config against new type: use provided config, or existing config
		configToValidate := input.Config
		if configToValidate == nil {
			configToValidate = json.RawMessage(rule.Config)
		}
		if _, err := guardrail.CreateEngine(*input.Type, configToValidate); err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidConfig, "invalid config for new type")
			return
		}
		rule.Type = *input.Type
	}
	if input.Direction != nil {
		if !validDirections[*input.Direction] {
			errorResp(c, http.StatusBadRequest, ErrInvalidDirection, "invalid direction, must be one of: request, response, both")
			return
		}
		rule.Direction = *input.Direction
	}
	if input.Enabled != nil {
		rule.Enabled = *input.Enabled
	}
	if input.Config != nil {
		if _, err := guardrail.CreateEngine(rule.Type, input.Config); err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidConfig, "invalid config")
			return
		}
		rule.Config = datatypes.JSON(input.Config)
	}
	if input.Severity != nil {
		if !validSeverities[*input.Severity] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid severity, must be one of: low, medium, high, critical"})
			return
		}
		rule.Severity = *input.Severity
	}
	if input.Action != nil {
		if !validActions[*input.Action] {
			errorResp(c, http.StatusBadRequest, ErrInvalidAction, "invalid action, must be one of: block, log, mask")
			return
		}
		rule.Action = *input.Action
	}
	if input.ModelFilter != nil {
		rule.ModelFilter = input.ModelFilter
	}

	if err := h.db.WithContext(c.Request.Context()).Save(&rule).Error; err != nil {
		internalErr(c, err, "update guardrail rule failed")
		return
	}

	h.svc.InvalidateCache()
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:update", "guardrail", fmt.Sprintf("%d", id), rule.Name, service.AuditDetail(map[string]any{"before": before, "after": map[string]any{"name": rule.Name, "type": rule.Type, "direction": rule.Direction, "enabled": rule.Enabled, "severity": rule.Severity, "action": rule.Action}}))
	}
	c.JSON(http.StatusOK, gin.H{"data": sanitizeRule(rule)})
}

func (h *GuardrailHandler) Delete(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	var rule guardrail.GuardrailRule
	q := h.db.WithContext(c.Request.Context())
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := h.db.WithContext(c.Request.Context()).Delete(&guardrail.GuardrailRule{}, id).Error; err != nil {
		internalErr(c, err, "delete guardrail rule failed")
		return
	}
	h.svc.InvalidateCache()
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:delete", "guardrail", fmt.Sprintf("%d", id), rule.Name, service.AuditDetail(map[string]any{"before": map[string]any{"name": rule.Name, "type": rule.Type}}))
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *GuardrailHandler) Test(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}

	var rule guardrail.GuardrailRule
	q := h.db.WithContext(c.Request.Context())
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&rule, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "rule not found"})
		return
	}

	var input struct {
		Text string `json:"text" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	eng, err := guardrail.CreateEngine(rule.Type, json.RawMessage(rule.Config))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create engine"})
		return
	}

	result, err := eng.Check(c.Request.Context(), input.Text, guardrail.DirectionRequest, "")
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "guardrail:test", "guardrail", fmt.Sprintf("%d", rule.ID), rule.Name, service.AuditDetail(map[string]any{"error": err.Error()}))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "check failed"})
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:test", "guardrail", fmt.Sprintf("%d", rule.ID), rule.Name, service.AuditDetail(map[string]any{"blocked": result.Blocked}))
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

func (h *GuardrailHandler) Stats(c *gin.Context) {
	since := time.Now().AddDate(0, 0, -30)
	if v := c.Query("start_time"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			since = t
		}
	}
	if maxSince := time.Now().AddDate(0, 0, -90); since.Before(maxSince) {
		since = maxSince
	}
	var stats []struct {
		GuardrailRule string `json:"guardrail_rule"`
		Count         int64  `json:"count"`
	}
	statsQ := h.db.WithContext(c.Request.Context()).
		Table("usage_logs").
		Select("guardrail_rule, COUNT(*) as count").
		Where("guardrail_triggered = ? AND created_at >= ?", true, since)
	if orgID := GetOrgID(c); orgID != 0 {
		statsQ = statsQ.Where("org_id = ?", orgID)
	}
	if err := statsQ.Group("guardrail_rule").
		Find(&stats).Error; err != nil {
		internalErr(c, err, "guardrail stats failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": stats})
}

func (h *GuardrailHandler) GetConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"enabled":   h.svc.IsEnabled(),
			"log_only":  h.svc.IsLogOnly(),
			"fail_open": h.svc.IsFailOpen(),
		},
	})
}

func (h *GuardrailHandler) UpdateConfig(c *gin.Context) {
	var input struct {
		Enabled  *bool `json:"enabled"`
		LogOnly  *bool `json:"log_only"`
		FailOpen *bool `json:"fail_open"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	before := map[string]any{"enabled": h.svc.IsEnabled(), "log_only": h.svc.IsLogOnly(), "fail_open": h.svc.IsFailOpen()}

	if input.Enabled != nil {
		if err := h.db.WithContext(c.Request.Context()).Save(&model.SystemSetting{Key: "guardrails_enabled", Value: boolToStr(*input.Enabled)}).Error; err != nil {
			internalErr(c, err, "save guardrails_enabled failed")
			return
		}
		h.svc.SetEnabled(*input.Enabled)
	}
	if input.LogOnly != nil {
		if err := h.db.WithContext(c.Request.Context()).Save(&model.SystemSetting{Key: "guardrails_log_only", Value: boolToStr(*input.LogOnly)}).Error; err != nil {
			internalErr(c, err, "save guardrails_log_only failed")
			return
		}
		h.svc.SetLogOnly(*input.LogOnly)
	}
	if input.FailOpen != nil {
		if err := h.db.WithContext(c.Request.Context()).Save(&model.SystemSetting{Key: "guardrails_fail_open", Value: boolToStr(*input.FailOpen)}).Error; err != nil {
			internalErr(c, err, "save guardrails_fail_open failed")
			return
		}
		h.svc.SetFailOpen(*input.FailOpen)
	}

	after := map[string]any{"enabled": h.svc.IsEnabled(), "log_only": h.svc.IsLogOnly(), "fail_open": h.svc.IsFailOpen()}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:update_config", "guardrail", "", "global_config", service.AuditDetail(map[string]any{"before": before, "after": after}))
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"enabled":   h.svc.IsEnabled(),
			"log_only":  h.svc.IsLogOnly(),
			"fail_open": h.svc.IsFailOpen(),
		},
	})
}

func boolToStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// sensitiveConfigKeys are stripped from guardrail config in API responses.
var sensitiveConfigKeys = []string{"api_key", "apikey", "secret", "token", "password"}

// sanitizeRuleConfig returns the rule config with sensitive fields masked recursively.
func sanitizeRuleConfig(raw datatypes.JSON) datatypes.JSON {
	if raw == nil {
		return raw
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	if sanitizeMap(m) {
		return raw
	}
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func sanitizeMap(m map[string]any) bool {
	changed := false
	for key, val := range m {
		if isSensitiveKey(key) {
			m[key] = "********"
			changed = true
		} else if sub, ok := val.(map[string]any); ok {
			if sanitizeMap(sub) {
				changed = true
			}
		}
	}
	return !changed
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, sk := range sensitiveConfigKeys {
		if lower == sk {
			return true
		}
	}
	return false
}

// sanitizeRule returns a sanitized copy of the rule for API responses.
func sanitizeRule(r guardrail.GuardrailRule) guardrail.GuardrailRule {
	r.Config = sanitizeRuleConfig(r.Config)
	return r
}

// sanitizeRules sanitizes a slice of rules.
func sanitizeRules(rules []guardrail.GuardrailRule) []guardrail.GuardrailRule {
	out := make([]guardrail.GuardrailRule, len(rules))
	for i := range rules {
		out[i] = sanitizeRule(rules[i])
	}
	return out
}
