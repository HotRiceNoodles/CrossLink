package admin

// M3-A rule versioning: append-only history + optimistic concurrent writes +
// rollback-as-new-version. Write discipline (same as the E sub-batch):
// rule row, version increment and history row commit in ONE transaction;
// NotifyRulesChanged fires only after a successful commit; a failed
// transaction must not change rules, history, or broadcast anything.
//
// Sensitive configs: history rows store the FULL original config (rollback
// must restore byte-exact state); every read path sanitizes via
// sanitizeRuleConfig, and audit details never carry config contents.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/service"
)

var errVersionConflict = fmt.Errorf("version conflict")

func actorFromContext(c *gin.Context) string {
	if v, ok := c.Get("username"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// snapshotVersion builds the history row for a rule state.
func snapshotVersion(r guardrail.GuardrailRule, version int64, changeKind, actor string) guardrail.GuardrailRuleVersion {
	return guardrail.GuardrailRuleVersion{
		RuleID:      r.ID,
		Version:     version,
		OrgID:       r.OrgID,
		Name:        r.Name,
		Type:        r.Type,
		Direction:   r.Direction,
		Enabled:     r.Enabled,
		Config:      r.Config,
		Severity:    r.Severity,
		Action:      r.Action,
		ModelFilter: r.ModelFilter,
		ChangeKind:  changeKind,
		Actor:       actor,
	}
}

// versionedCreate persists rule (version 1) plus its v1 history row in one
// transaction.
func versionedCreate(c *gin.Context, db *gorm.DB, rule *guardrail.GuardrailRule) error {
	actor := actorFromContext(c)
	rule.Version = 1
	return db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(rule).Error; err != nil {
			return err
		}
		hist := snapshotVersion(*rule, 1, "create", actor)
		return tx.Create(&hist).Error
	})
}

// versionedUpdate applies the staged field updates optimistically: the
// UPDATE re-checks the loaded version, bumps version by one and writes the
// history row for the new state — all in one transaction. Returns
// errVersionConflict when a concurrent writer won.
func versionedUpdate(c *gin.Context, db *gorm.DB, rule *guardrail.GuardrailRule, updates map[string]any) error {
	actor := actorFromContext(c)
	fromVersion := rule.Version
	newVersion := fromVersion + 1
	if updates == nil {
		updates = map[string]any{}
	}
	for k, v := range map[string]any{
		"name": rule.Name, "type": rule.Type, "direction": rule.Direction,
		"enabled": rule.Enabled, "config": rule.Config, "severity": rule.Severity,
		"action": rule.Action, "model_filter": rule.ModelFilter,
		"updated_at": time.Now(),
	} {
		updates[k] = v
	}
	updates["version"] = gorm.Expr("version + 1")
	err := db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&guardrail.GuardrailRule{}).
			Where("id = ? AND version = ?", rule.ID, fromVersion).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errVersionConflict
		}
		rule.Version = newVersion
		hist := snapshotVersion(*rule, newVersion, "update", actor)
		return tx.Create(&hist).Error
	})
	if err == errVersionConflict {
		rule.Version = fromVersion
	}
	return err
}

// versionedDelete records the terminal snapshot (delete kind, next version
// number — the chain stays unique) and removes the rule, in one transaction.
func versionedDelete(c *gin.Context, db *gorm.DB, rule *guardrail.GuardrailRule) error {
	actor := actorFromContext(c)
	fromVersion := rule.Version
	return db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		hist := snapshotVersion(*rule, fromVersion+1, "delete", actor)
		if err := tx.Create(&hist).Error; err != nil {
			return err
		}
		res := tx.Where("id = ? AND version = ?", rule.ID, fromVersion).Delete(&guardrail.GuardrailRule{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errVersionConflict
		}
		return nil
	})
}

// applyVersionToRule maps a history row back onto the rule's mutable fields.
func applyVersionToRule(rule *guardrail.GuardrailRule, h guardrail.GuardrailRuleVersion) {
	rule.Name = h.Name
	rule.Type = h.Type
	rule.Direction = h.Direction
	rule.Enabled = h.Enabled
	rule.Config = h.Config
	rule.Severity = h.Severity
	rule.Action = h.Action
	rule.ModelFilter = h.ModelFilter
}

// Rollback re-publishes a historical snapshot as a NEW version (history is
// never edited). Optimistic concurrency: the current version is re-checked in
// the UPDATE's WHERE clause, so a concurrent change yields 409 instead of a
// lost update. Org scoping follows the current-rule load (cross-tenant rules
// 404 before any history access). Raw historical config is consumed only
// here, server-internally, under the same permission as rule update.
func (h *GuardrailHandler) Rollback(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}
	var input struct {
		ToVersion int64 `json:"to_version" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
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

	// history lookup is tenant-bounded by the rule load above
	var hist guardrail.GuardrailRuleVersion
	if err := h.db.WithContext(c.Request.Context()).
		Where("rule_id = ? AND version = ?", rule.ID, input.ToVersion).
		First(&hist).Error; err != nil {
		errorResp(c, http.StatusNotFound, ErrRuleNotFound, "version not found")
		return
	}
	// a delete snapshot is terminal: it documents removal, not a restorable state
	if hist.ChangeKind == "delete" {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "cannot roll back to a delete snapshot")
		return
	}
	// validate the historical config before publishing it
	if _, err := guardrail.CreateEngine(hist.Type, json.RawMessage(hist.Config)); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidConfig, fmt.Sprintf("historical config no longer valid: %v", err))
		return
	}

	fromVersion := rule.Version
	newVersion := fromVersion + 1
	applyVersionToRule(&rule, hist)
	actor := actorFromContext(c)

	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&guardrail.GuardrailRule{}).
			Where("id = ? AND version = ?", id, fromVersion).
			Updates(map[string]any{
				"name": rule.Name, "type": rule.Type, "direction": rule.Direction,
				"enabled": rule.Enabled, "config": rule.Config, "severity": rule.Severity,
				"action": rule.Action, "model_filter": rule.ModelFilter,
				"updated_at": time.Now(), "version": gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errVersionConflict
		}
		rule.Version = newVersion
		hist := snapshotVersion(rule, newVersion, "rollback", actor)
		return tx.Create(&hist).Error
	})
	if err == errVersionConflict {
		c.JSON(http.StatusConflict, gin.H{"error": "version conflict: rule changed concurrently, reload and retry", "error_code": "version_conflict"})
		return
	}
	if err != nil {
		internalErr(c, err, "rollback guardrail rule failed")
		return
	}

	h.svc.NotifyRulesChanged(c.Request.Context()) // commit succeeded → local invalidate + broadcast
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "guardrail:rollback", "guardrail", fmt.Sprintf("%d", id), rule.Name,
			service.AuditDetail(map[string]any{"from_version": fromVersion, "to_base_version": input.ToVersion, "new_version": newVersion}))
	}
	c.JSON(http.StatusOK, gin.H{"data": sanitizeRule(rule)})
}

// Versions lists a rule's history, newest first. Tenant-bounded by the
// org-scoped rule load; configs are sanitized (sensitive keys masked).
func (h *GuardrailHandler) Versions(c *gin.Context) {
	ruleID := parseID(c.Query("rule_id"))
	if ruleID == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "rule_id required")
		return
	}
	var rule guardrail.GuardrailRule
	q := h.db.WithContext(c.Request.Context())
	if orgID := GetOrgID(c); orgID != 0 {
		q = q.Where("org_id = ?", orgID)
	}
	if err := q.First(&rule, ruleID).Error; err != nil {
		errorResp(c, http.StatusNotFound, ErrRuleNotFound, "rule not found")
		return
	}
	var rows []guardrail.GuardrailRuleVersion
	if err := h.db.WithContext(c.Request.Context()).
		Where("rule_id = ?", ruleID).
		Order("version DESC").
		Find(&rows).Error; err != nil {
		internalErr(c, err, "list rule versions failed")
		return
	}
	for i := range rows {
		rows[i].Config = sanitizeRuleConfig(rows[i].Config) // sensitive keys never leave the server
	}
	c.JSON(http.StatusOK, gin.H{"data": rows, "current_version": rule.Version})
}
