package admin

import (
	"encoding/json"
	"github.com/crosslink/internal/guardrail"
	"github.com/gin-gonic/gin"
)

func (h *GuardrailHandler) validateReviewRule(c *gin.Context, r *guardrail.GuardrailRule) bool {
	if r.Type != guardrail.ModelReview {
		return true
	}
	var cfg guardrail.ReviewConfig
	if r.Direction != "request" || (r.Action != "block" && r.Action != "log") || json.Unmarshal(r.Config, &cfg) != nil {
		errorResp(c, 400, ErrInvalidConfig, "模型审核仅支持请求正文的记录或阻断")
		return false
	}
	// 社区/Pro 单租户 admin 建的规则无组织作用域（OrgID=nil，guardrail.go 同款
	// 语义），按 org=0 与审核服务对齐；企业档规则带组织，仍校验同组织服务。
	org := int64(0)
	if r.OrgID != nil {
		org = *r.OrgID
	}
	var count int64
	if err := h.db.WithContext(c.Request.Context()).Model(&guardrail.ReviewService{}).Where("id=? AND org_id=?", cfg.ServiceID, org).Count(&count).Error; err != nil {
		internalErr(c, err, "validate review service")
		return false
	}
	if count != 1 {
		errorResp(c, 400, ErrInvalidConfig, "所选审核服务不可用")
		return false
	}
	return true
}

func (h *GuardrailHandler) listReviewServices(c *gin.Context) {
	var rows []guardrail.ReviewService
	if err := h.db.WithContext(c.Request.Context()).Where("org_id=?", GetOrgID(c)).Order("id").Find(&rows).Error; err != nil {
		internalErr(c, err, "list review services")
		return
	}
	c.JSON(200, gin.H{"data": rows})
}

func (h *GuardrailHandler) reviewCatalog(c *gin.Context) {
	var rows []guardrail.ReviewCatalogEntry
	err := guardrail.ReviewCatalog(h.db.WithContext(c.Request.Context()), GetOrgID(c)).Select("provider_models.id,p.display_name AS provider,provider_models.provider_model AS model").Order("provider_models.id").Scan(&rows).Error
	if err != nil {
		internalErr(c, err, "list review catalog")
		return
	}
	c.JSON(200, gin.H{"data": rows})
}

func (h *GuardrailHandler) connectReview(c *gin.Context) {
	var in struct {
		Name            string  `json:"name" binding:"required,max=128"`
		Kind            string  `json:"kind"`
		ProviderModelID int64   `json:"provider_model_id"`
		Endpoint        string  `json:"endpoint" binding:"max=1024"`
		Token           string  `json:"token" binding:"max=8192"`
		ModelVersion    string  `json:"model_version"`
		Threshold       float64 `json:"threshold"`
		TimeoutMS       int     `json:"timeout_ms"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		errorResp(c, 400, ErrInvalidConfig, "审核服务配置无效")
		return
	}
	r := guardrail.ReviewService{OrgID: GetOrgID(c), Name: in.Name, Kind: in.Kind, ProviderModelID: in.ProviderModelID, Endpoint: in.Endpoint, ModelVersion: in.ModelVersion, Threshold: in.Threshold, TimeoutMS: in.TimeoutMS}
	if r.TimeoutMS == 0 {
		r.TimeoutMS = 1500
	}
	if err := h.svc.ConnectReview(c.Request.Context(), &r, in.Token); err != nil {
		errorResp(c, 400, ErrInvalidConfig, err.Error())
		return
	}
	c.JSON(201, gin.H{"data": r})
}

func (h *GuardrailHandler) reviewHealth(c *gin.Context) {
	var r guardrail.ReviewService
	if err := h.db.WithContext(c.Request.Context()).Where("org_id=?", GetOrgID(c)).First(&r, parseID(c.Param("id"))).Error; err != nil {
		c.Status(404)
		return
	}
	c.JSON(200, gin.H{"data": h.svc.ReviewHealth(c.Request.Context(), r)})
}
