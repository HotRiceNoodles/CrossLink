package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

// PATValidator is the consumer-side interface of PatService used by PAT auth.
type PATValidator interface {
	Validate(ctx context.Context, plaintext string) (*model.PatToken, error)
	TouchLastUsed(ctx context.Context, id int64) error
}

// UserResolver is the consumer-side interface of repository.UserRepo.
type UserResolver interface {
	GetByID(ctx context.Context, id int64) (*model.User, error)
}

// PATAuthMiddleware authenticates requests via a Personal Access Token
// (Bearer clpat_...) and enforces the token's scopes for the given action.
//
// Unlike JWTAuthMiddleware it never reads cookies (CSRF defense): PATs are
// machine-issued secrets passed only via the Authorization header.
//
// Step 4 simplification: only UserRepo is injected; org_id is always 0.
// Admin users have no org/team anyway, and non-admin PAT users don't exist
// in Phase 1 (pat:manage is admin-exclusive). Revisit when Phase 3 opens
// PATs to regular users.
func PATAuthMiddleware(patSvc PATValidator, users UserResolver, action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. Authorization header only, "Bearer clpat_" prefix.
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer "+service.PatTokenPrefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authorization"})
			return
		}
		token := strings.TrimPrefix(header, "Bearer ")
		if len(token) <= len(service.PatTokenPrefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid authorization"})
			return
		}

		// 2. Validate — uniform 401, no reason leakage (anti-enumeration).
		tok, err := patSvc.Validate(c.Request.Context(), token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		// 3. Scope check — fail-closed on unparseable scopes.
		scopes, err := tok.ScopeList()
		if err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			return
		}
		allowed := false
		for _, sc := range scopes {
			if sc == action {
				allowed = true
				break
			}
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			return
		}

		// 4. Fresh user lookup — reject missing or disabled users.
		user, err := users.GetByID(c.Request.Context(), tok.UserID)
		if err != nil || user.Status == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		// 5. Context injection — keys must match the JWT chain exactly so
		// downstream RequireAction/handlers work unchanged.
		c.Set("user_id", user.ID)
		c.Set("username", user.Username)
		c.Set("role_id", user.RoleID)
		c.Set("role_name", user.Role.Name)
		c.Set("org_id", int64(0))
		c.Set("pat_id", tok.ID)
		c.Set("pat_scopes", scopes)

		// Async last-used touch — never block the request path.
		patID := tok.ID
		go func() {
			_ = patSvc.TouchLastUsed(context.Background(), patID)
		}()

		c.Next()
	}
}
