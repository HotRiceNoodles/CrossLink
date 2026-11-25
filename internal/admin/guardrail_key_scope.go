package admin

import (
	"net/http"
	"slices"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// manageableKeys 圈定当前会话有权管理的 Key。org=0 表示无租户作用域（社区/Pro
// 单租户的 admin 上下文，或企业平台 admin 的全局视角），不过滤组织——与
// providers.go / guardrail.go 列表查询的 org=0 惯例一致；org>0 精确到租户。
func manageableKeys(c *gin.Context, db *gorm.DB, org int64) *gorm.DB {
	q := db.WithContext(c.Request.Context()).Model(&model.APIKey{}).Where("status = 1 AND (expires_at IS NULL OR expires_at > ?)", time.Now())
	if org > 0 {
		q = q.Where("org_id = ?", org)
	}
	permission := db.Model(&model.RolePermission{}).Select("1").Where("role_id=? AND action=?", GetRoleID(c), "key:update")
	q = q.Where("EXISTS (?)", permission)
	if !IsAdmin(c) {
		user := GetUserID(c)
		teams := db.Model(&model.TeamMember{}).Select("team_id").Where("user_id = ?", user)
		q = q.Where("(team_id IS NULL AND created_by_id = ?) OR team_id IN (?)", user, teams)
	}
	return q
}

func (h *GuardrailHandler) setScope(c *gin.Context, rule *guardrail.GuardrailRule, mode *string, ids *[]int64) bool {
	if mode != nil {
		rule.ScopeMode = *mode
	}
	if ids != nil {
		rule.APIKeyIDs = *ids
	}
	if rule.ScopeMode == "" && mode == nil {
		rule.ScopeMode = "all"
	}
	switch rule.ScopeMode {
	case "all":
		if len(rule.APIKeyIDs) == 0 {
			return true
		}
	case "api_keys":
		if len(rule.APIKeyIDs) > 0 {
			// 单租户 admin 建的规则无组织作用域（OrgID=nil，与 guardrail.go 的
			// 赋值语义一致），按 org=0 校验即无租户过滤（manageableKeys 口径）；
			// 企业档规则带组织，仍按同组织校验。
			org := int64(0)
			if rule.OrgID != nil {
				org = *rule.OrgID
			}
			var count int64
			if err := manageableKeys(c, h.db, org).Where("id IN ?", []int64(rule.APIKeyIDs)).Count(&count).Error; err != nil {
				internalErr(c, err, "validate key scope")
				return false
			}
			if count == int64(len(rule.APIKeyIDs)) {
				return true
			}
		}
	}
	errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "请选择同组织有权管理的有效 API Key")
	return false
}

// CanBindGuardKey 验证接入凭证管理者所选 Key 的组织和管理范围。
func CanBindGuardKey(c *gin.Context, db *gorm.DB, org, id int64) bool {
	var n int64
	return id > 0 && manageableKeys(c, db, org).Where("id=?", id).Count(&n).Error == nil && n == 1
}

func widensKeyScope(current guardrail.GuardrailRule, target guardrail.GuardrailRuleVersion) bool {
	if current.ScopeMode != "api_keys" {
		return false
	}
	if target.ScopeMode != "api_keys" {
		return true
	}
	for _, id := range target.APIKeyIDs {
		if !slices.Contains(current.APIKeyIDs, id) {
			return true
		}
	}
	return false
}
