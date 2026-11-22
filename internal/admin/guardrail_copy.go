package admin

import (
	"encoding/json"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
)

// 复制使用服务端快照，避免从已脱敏的列表响应重建引擎配置。
func (h *GuardrailHandler) CopyRule(c *gin.Context) {
	var input struct {
		Name string `json:"name" binding:"required,max=255"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, 400, ErrInvalidRequest, "请填写副本名称")
		return
	}
	var r guardrail.GuardrailRule
	q := h.db.WithContext(c.Request.Context())
	if org := GetOrgID(c); org != 0 {
		q = q.Where("org_id=?", org)
	}
	if err := q.First(&r, parseID(c.Param("id"))).Error; err != nil {
		errorResp(c, 404, ErrRuleNotFound, "规则不存在")
		return
	}
	if !h.setScope(c, &r, nil, nil) || !h.validateReviewRule(c, &r) {
		return
	}
	if _, err := guardrail.CreateEngine(r.Type, json.RawMessage(r.Config)); err != nil {
		errorResp(c, 400, ErrInvalidConfig, "源配置无效")
		return
	}
	if err := guardrail.ValidateRuleAction(r.Type, r.Action); err != nil {
		errorResp(c, 400, ErrInvalidAction, err.Error())
		return
	}
	r.ID, r.Version, r.Enabled, r.Name = 0, 1, false, input.Name
	r.CreatedAt, r.UpdatedAt = time.Time{}, time.Time{}
	if err := versionedCreate(c, h.db, &r); err != nil {
		internalErr(c, err, "copy guardrail rule")
		return
	}
	c.JSON(201, gin.H{"data": sanitizeRule(r)})
}
