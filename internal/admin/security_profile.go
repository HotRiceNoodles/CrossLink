package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/license"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (h *GuardrailHandler) RegisterPromptSecurity(group *gin.RouterGroup, read, write, bind gin.HandlerFunc) {
	// 审核服务侧（含 /keys：规则编辑器按 Key 限定范围时取可选 Key 列表）。
	// 企业档仍要求租户上下文；社区/Pro 单租户 org=0 是常态（对齐 org_scope.go
	// denyNoOrgScope 口径），放行——社区档由 RequireAction 的 guardrail:*
	// 动作位（Pro 集）拦截。2026-11-24 审核模型下放 Pro。
	gReview := group.Group("/prompt-security", func(c *gin.Context) {
		if GetOrgID(c) == 0 && license.G().CurrentTier() == license.TierEnterprise {
			c.AbortWithStatusJSON(403, gin.H{"error": "请先选择组织"})
			return
		}
		c.Next()
	})
	gReview.GET("/review-services", read, h.listReviewServices)
	gReview.GET("/catalog", read, h.reviewCatalog)
	gReview.GET("/review-services/:id/health", read, h.reviewHealth)
	gReview.POST("/review-services", write, h.connectReview)
	gReview.GET("/keys", read, h.profileKeys)
	// 应用场景侧：数据按组织隔离，仅企业多租户具备组织上下文，
	// Pro/社区 org=0 不可用（2026-11-24 产品口径：社区/Pro 隐藏）。
	gProfile := group.Group("/prompt-security", func(c *gin.Context) {
		if GetOrgID(c) <= 0 {
			c.AbortWithStatusJSON(403, gin.H{"error": "请先选择组织"})
			return
		}
		c.Next()
	})
	gProfile.GET("/profiles", read, h.listProfiles)
	gProfile.POST("/profiles", write, h.saveProfile)
	gProfile.PUT("/profiles/:id", write, h.saveProfile)
	gProfile.POST("/profiles/:id/publish", write, h.publishProfile)
	gProfile.GET("/profiles/:id/versions", read, h.profileHistory)
	gProfile.PUT("/bindings", write, bind, h.bindProfile)
	gProfile.DELETE("/profiles/:id", write, bind, h.deleteProfile)
}

func (h *GuardrailHandler) profileKeys(c *gin.Context) {
	var rows []struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		KeyPrefix string `json:"key_prefix"`
		ProfileID *int64 `json:"profile_id"`
	}
	err := manageableKeys(c, h.db, GetOrgID(c)).Select("api_keys.id, api_keys.name, api_keys.key_prefix, b.profile_id").
		Joins("LEFT JOIN security_profile_bindings b ON b.api_key_id=api_keys.id").Order("api_keys.id").Find(&rows).Error
	if err != nil {
		internalErr(c, err, "list manageable keys")
		return
	}
	c.JSON(200, gin.H{"data": rows})
}

func (h *GuardrailHandler) listProfiles(c *gin.Context) {
	var rows []struct {
		guardrail.SecurityProfile
		BoundKeyCount int64 `json:"bound_key_count"`
		// 取最新版本的 created_at：SQLite 驱动返回字符串、PostgreSQL 返回 time.Time，
		// 统一按字符串下发，前端按本地时间格式化。
		UpdatedAt *string `json:"updated_at"`
	}
	if err := h.db.WithContext(c.Request.Context()).Model(&guardrail.SecurityProfile{}).
		Select("security_profiles.*, "+
			"(SELECT count(*) FROM security_profile_bindings b WHERE b.profile_id=security_profiles.id) AS bound_key_count, "+
			"(SELECT max(v.created_at) FROM security_profile_versions v WHERE v.profile_id=security_profiles.id) AS updated_at").
		Where("org_id=?", GetOrgID(c)).Order("id").Find(&rows).Error; err != nil {
		internalErr(c, err, "list profiles")
		return
	}
	c.JSON(200, gin.H{"data": rows})
}

func (h *GuardrailHandler) saveProfile(c *gin.Context) {
	var input struct {
		guardrail.ProfileContent
		Revision int64 `json:"revision"`
	}
	if err := c.ShouldBindJSON(&input); err != nil || strings.TrimSpace(input.Name) == "" || strings.TrimSpace(input.Purpose) == "" {
		errorResp(c, 400, ErrInvalidRequest, "请填写名称和应用用途")
		return
	}
	p := guardrail.SecurityProfile{OrgID: GetOrgID(c), Revision: 1, ProfileContent: input.ProfileContent}
	db := h.db.WithContext(c.Request.Context())
	if c.Param("id") == "" {
		if err := db.Create(&p).Error; err != nil {
			internalErr(c, err, "create profile")
			return
		}
		c.JSON(201, gin.H{"data": p})
		return
	}
	result := db.Model(&p).Where("id=? AND org_id=? AND revision=?", parseID(c.Param("id")), p.OrgID, input.Revision).
		Updates(map[string]any{"name": input.Name, "purpose": input.Purpose, "allowed_tasks": input.AllowedTasks, "forbidden": input.Forbidden, "data_boundary": input.DataBoundary, "revision": gorm.Expr("revision+1")})
	if result.Error != nil {
		internalErr(c, result.Error, "save profile")
		return
	}
	if result.RowsAffected != 1 {
		c.JSON(409, gin.H{"error": "版本已变化，请刷新"})
		return
	}
	c.JSON(200, gin.H{"revision": input.Revision + 1})
}

func (h *GuardrailHandler) publishProfile(c *gin.Context) {
	var input struct {
		Revision int64 `json:"revision" binding:"required"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, 400, ErrInvalidRequest, "缺少草稿版本")
		return
	}
	var version guardrail.SecurityProfileVersion
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var p guardrail.SecurityProfile
		if err := tx.Where("id=? AND org_id=? AND revision=?", parseID(c.Param("id")), GetOrgID(c), input.Revision).Take(&p).Error; err != nil {
			return err
		}
		published := p.Published + 1
		result := tx.Model(&p).Where("revision=? AND published=?", input.Revision, p.Published).Updates(map[string]any{"published": p.Published + 1, "revision": p.Revision + 1})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errVersionConflict
		}
		version = guardrail.SecurityProfileVersion{ProfileID: p.ID, Version: published, ProfileContent: p.ProfileContent, Actor: actorFromContext(c)}
		return tx.Create(&version).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, errVersionConflict) {
		c.JSON(409, gin.H{"error": "版本已变化，请刷新"})
		return
	}
	if err != nil {
		internalErr(c, err, "publish profile")
		return
	}
	c.JSON(200, gin.H{"data": version})
}

func (h *GuardrailHandler) profileHistory(c *gin.Context) {
	var rows []guardrail.SecurityProfileVersion
	err := h.db.WithContext(c.Request.Context()).Joins("JOIN security_profiles p ON p.id=security_profile_versions.profile_id").
		Where("p.org_id=? AND p.id=?", GetOrgID(c), parseID(c.Param("id"))).Order("version DESC").Find(&rows).Error
	if err != nil {
		internalErr(c, err, "list profile versions")
		return
	}
	c.JSON(200, gin.H{"data": rows})
}

func (h *GuardrailHandler) bindProfile(c *gin.Context) {
	var in struct {
		ProfileID *int64  `json:"profile_id" binding:"required"`
		IDs       []int64 `json:"api_key_ids" binding:"required,min=1"`
	}
	if err := c.ShouldBindJSON(&in); err != nil || *in.ProfileID < 0 {
		errorResp(c, 400, ErrInvalidRequest, "请选择配置和 API Key")
		return
	}
	org := GetOrgID(c)
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var ids []int64
		if err := manageableKeys(c, tx, org).Where("id IN ?", in.IDs).Order("id").Clauses(clause.Locking{Strength: "UPDATE"}).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) != len(in.IDs) {
			return gorm.ErrRecordNotFound
		}
		if *in.ProfileID == 0 {
			return tx.Where("api_key_id IN ?", ids).Delete(&guardrail.SecurityProfileBinding{}).Error
		}
		var profile guardrail.SecurityProfile
		if err := tx.Where("org_id=? AND published>0", org).First(&profile, *in.ProfileID).Error; err != nil {
			return err
		}
		for _, id := range ids {
			b := guardrail.SecurityProfileBinding{APIKeyID: id, ProfileID: profile.ID}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "api_key_id"}}, DoUpdates: clause.AssignmentColumns([]string{"profile_id"})}).Create(&b).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		errorResp(c, 400, ErrInvalidRequest, "配置未发布或 API Key 不可用")
		return
	}
	if err != nil {
		internalErr(c, err, "bind profile")
		return
	}
	c.Status(http.StatusNoContent)
}

var errProfileBound = errors.New("profile still bound to api keys")

// deleteProfile 只允许删除没有任何 Key 绑定的配置。已绑定时必须先在密钥侧解绑，
// 避免一次删除把 Key 的应用背景静默清空，也让审计能对应上是谁解绑的。
func (h *GuardrailHandler) deleteProfile(c *gin.Context) {
	id := parseID(c.Param("id"))
	org := GetOrgID(c)
	var bound int64
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var p guardrail.SecurityProfile
		if err := tx.Where("org_id=? AND id=?", org, id).Clauses(clause.Locking{Strength: "UPDATE"}).Take(&p).Error; err != nil {
			return err
		}
		if err := tx.Model(&guardrail.SecurityProfileBinding{}).Where("profile_id=?", id).Count(&bound).Error; err != nil {
			return err
		}
		if bound > 0 {
			return errProfileBound
		}
		if err := tx.Where("profile_id=?", id).Delete(&guardrail.SecurityProfileVersion{}).Error; err != nil {
			return err
		}
		return tx.Delete(&p).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		errorResp(c, 404, ErrNotFound, "配置不存在")
		return
	}
	if errors.Is(err, errProfileBound) {
		c.JSON(http.StatusConflict, gin.H{"error": "仍有 " + strconv.FormatInt(bound, 10) + " 把 Key 绑定该配置，请先在密钥列表解绑"})
		return
	}
	if err != nil {
		internalErr(c, err, "delete profile")
		return
	}
	c.Status(http.StatusNoContent)
}
