package admin

// profile.go — 个人中心自助资料（GET/PUT /user/profile）。
// users 表本就有 email/display_name/created_at/last_login_at，此前缺 self-service 出口，
// 个人中心只能拿登录快照（无邮箱/注册时间）。只开放 display_name/email 自助修改；
// 角色/组织/状态属管理面语义（user:update），不混入自助通道。

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

type ProfileHandler struct {
	userRepo *repository.UserRepo
	auditSvc *service.AuditService
}

func NewProfileHandler(userRepo *repository.UserRepo, auditSvc *service.AuditService) *ProfileHandler {
	return &ProfileHandler{userRepo: userRepo, auditSvc: auditSvc}
}

// profileDTO 白名单输出（不携带 password_hash/org 上下文——org 以登录态为准，由前端结合展示）。
func profileDTO(username, displayName, roleName string, userID int64, email *string, createdAt, lastLoginAt any, forceChange bool) gin.H {
	dto := gin.H{
		"id":                    userID,
		"username":              username,
		"display_name":          displayName,
		"role_name":             roleName,
		"created_at":            createdAt,
		"last_login_at":         lastLoginAt,
		"force_password_change": forceChange,
	}
	if email != nil && *email != "" {
		dto["email"] = *email
	}
	return dto
}

func (h *ProfileHandler) Get(c *gin.Context) {
	userID := GetUserID(c)
	if userID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	u, err := h.userRepo.GetByID(c.Request.Context(), userID)
	if err != nil {
		internalErr(c, err, "get profile failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": profileDTO(u.Username, u.DisplayName, u.Role.Name, u.ID, u.Email, u.CreatedAt, u.LastLoginAt, u.ForcePasswordChange)})
}

func (h *ProfileHandler) Update(c *gin.Context) {
	userID := GetUserID(c)
	if userID <= 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "user:update_profile", "user", "", "", service.AuditDetail(map[string]any{"reason": "unauthorized"}))
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var input struct {
		DisplayName *string `json:"display_name"`
		Email       *string `json:"email"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "user:update_profile", "user", fmt.Sprintf("%d", userID), "", service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	u, err := h.userRepo.GetByID(c.Request.Context(), userID)
	if err != nil {
		internalErr(c, err, "get profile failed")
		return
	}

	cols := map[string]any{}
	changed := []string{}
	if input.DisplayName != nil {
		name := strings.TrimSpace(*input.DisplayName)
		if name == "" || len(name) > 128 {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "user:update_profile", "user", fmt.Sprintf("%d", userID), name, service.AuditDetail(map[string]any{"reason": "invalid_display_name"}))
			}
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "display_name must be 1-128 characters")
			return
		}
		if name != u.DisplayName {
			cols["display_name"] = name
			changed = append(changed, "display_name")
		}
	}
	if input.Email != nil {
		email := strings.TrimSpace(*input.Email)
		if email != "" && (len(email) > 255 || !strings.Contains(email, "@")) {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "user:update_profile", "user", fmt.Sprintf("%d", userID), u.DisplayName, service.AuditDetail(map[string]any{"reason": "invalid_email"}))
			}
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid email")
			return
		}
		cur := ""
		if u.Email != nil {
			cur = *u.Email
		}
		if email != cur {
			cols["email"] = email
			changed = append(changed, "email")
		}
	}

	if len(changed) > 0 {
		/* 定向列更新：不带关联 Save，避免 Preload("Role") 的关联被误写 */
		if err := h.userRepo.UpdateColumnsByID(c.Request.Context(), userID, cols); err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "user:update_profile", "user", fmt.Sprintf("%d", userID), u.DisplayName, service.AuditDetail(map[string]any{"reason": "save_failed", "error": err.Error()}))
			}
			internalErr(c, err, "update profile failed")
			return
		}
		if h.auditSvc != nil {
			name := u.DisplayName
			if v, ok := cols["display_name"].(string); ok {
				name = v
			}
			h.auditSvc.LogFromContext(c, "user:update_profile", "user", fmt.Sprintf("%d", userID), name, service.AuditDetail(map[string]any{"fields": changed}))
		}
	}

	u2, err := h.userRepo.GetByID(c.Request.Context(), userID)
	if err != nil {
		internalErr(c, err, "get profile failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": profileDTO(u2.Username, u2.DisplayName, u2.Role.Name, u2.ID, u2.Email, u2.CreatedAt, u2.LastLoginAt, u2.ForcePasswordChange)})
}
