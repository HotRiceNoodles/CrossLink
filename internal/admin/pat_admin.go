package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
)

// PatSvc is the service subset used by PATAdminHandler.
type PatSvc interface {
	Create(ctx context.Context, userID int64, allowedActions []string, name string, scopes []string) (*service.CreatePatResult, error)
	Revoke(ctx context.Context, id, userID int64) error
}

// PatLister lists PATs owned by a user.
type PatLister interface {
	ListByUser(ctx context.Context, userID int64) ([]model.PatToken, error)
}

// PatRolePerms resolves a role's DB permission actions.
type PatRolePerms interface {
	GetPermissions(ctx context.Context, roleID int64) ([]string, error)
}

type PATAdminHandler struct {
	patSvc    PatSvc
	lister    PatLister
	rolePerms PatRolePerms
	auditSvc  *service.AuditService
}

func NewPATAdminHandler(patSvc PatSvc, lister PatLister, rolePerms PatRolePerms, auditSvc *service.AuditService) *PATAdminHandler {
	return &PATAdminHandler{patSvc: patSvc, lister: lister, rolePerms: rolePerms, auditSvc: auditSvc}
}

// allowedActions resolves the caller's effective actions from their role.
func (h *PATAdminHandler) allowedActions(c *gin.Context) []string {
	dbActions, err := h.rolePerms.GetPermissions(c.Request.Context(), GetRoleID(c))
	if err != nil {
		return nil
	}
	return license.EffectiveActions(dbActions)
}

func (h *PATAdminHandler) Create(c *gin.Context) {
	var input struct {
		Name   string   `json:"name" binding:"required"`
		Scopes []string `json:"scopes"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}

	result, err := h.patSvc.Create(c.Request.Context(), GetUserID(c), h.allowedActions(c), input.Name, input.Scopes)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrScopeExceeded):
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "pat:create", "pat", "", input.Name, service.AuditDetail(map[string]any{"via": "admin-ui", "scopes": input.Scopes, "reason": "scope_exceeded"}))
			}
			errorResp(c, http.StatusBadRequest, ErrInvalidScope, "scope exceeds your permissions")
		case errors.Is(err, service.ErrInvalidName):
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "name is required")
		default:
			internalErr(c, err, "create pat failed")
		}
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "pat:create", "pat", fmt.Sprintf("%d", result.Token.ID), result.Token.Name, service.AuditDetail(map[string]any{"via": "admin-ui", "scopes": input.Scopes}))
	}
	c.JSON(http.StatusCreated, gin.H{
		"data": gin.H{
			"token": result.Plaintext,
			"pat":   patDTO(result.Token),
		},
	})
}

func (h *PATAdminHandler) List(c *gin.Context) {
	toks, err := h.lister.ListByUser(c.Request.Context(), GetUserID(c))
	if err != nil {
		internalErr(c, err, "list pats failed")
		return
	}
	out := make([]gin.H, 0, len(toks))
	for i := range toks {
		out = append(out, patDTO(&toks[i]))
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func (h *PATAdminHandler) Revoke(c *gin.Context) {
	id := parseID(c.Param("id"))
	if id == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidID, "invalid id")
		return
	}

	if err := h.patSvc.Revoke(c.Request.Context(), id, GetUserID(c)); err != nil {
		if errors.Is(err, service.ErrPatTokenNotFound) {
			errorResp(c, http.StatusNotFound, ErrNotFound, "pat not found")
			return
		}
		internalErr(c, err, "revoke pat failed")
		return
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "pat:revoke", "pat", fmt.Sprintf("%d", id), "", service.AuditDetail(map[string]any{"via": "admin-ui"}))
	}
	c.JSON(http.StatusOK, gin.H{"message": "revoked"})
}

// patDTO builds the whitelisted PAT response — never exposes token_hash.
func patDTO(t *model.PatToken) gin.H {
	scopes, err := t.ScopeList()
	if err != nil {
		scopes = nil
	}
	return gin.H{
		"id":           t.ID,
		"name":         t.Name,
		"scopes":       scopes,
		"status":       t.Status,
		"expires_at":   t.ExpiresAt,
		"last_used_at": t.LastUsedAt,
		"created_at":   t.CreatedAt,
	}
}
