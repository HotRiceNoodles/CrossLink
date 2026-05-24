package admin

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const bcryptCost = 12

type Claims struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	RoleID   int64  `json:"role_id"`
	RoleName string `json:"role_name"`
	TeamID   int64  `json:"team_id,omitempty"`
	jwt.RegisteredClaims
}

func GenerateToken(user *model.User, roleName string, teamID int64, cfg config.AdminConfig, cp crypto.CryptoProvider) (string, error) {
	expiry := time.Duration(cfg.TokenExpiry) * time.Hour
	claims := Claims{
		UserID:   user.ID,
		Username: user.Username,
		RoleID:   user.RoleID,
		RoleName: roleName,
		TeamID:   teamID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(expiry)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			Issuer:    "crosslink",
		},
	}
	token := jwt.NewWithClaims(cp.JWTSigningMethod(), claims)
	return token.SignedString([]byte(cfg.JWTSecret))
}

func JWTAuthMiddleware(cfg config.AdminConfig, db *gorm.DB, cp crypto.CryptoProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Try cookie first, fall back to Authorization header
		tokenStr, _ := c.Cookie("admin_token")
		if tokenStr == "" {
			tokenStr = c.GetHeader("Authorization")
			if strings.HasPrefix(tokenStr, "Bearer ") {
				tokenStr = strings.TrimPrefix(tokenStr, "Bearer ")
			}
		}

		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (any, error) {
			switch t.Method.Alg() {
			case "HS256", "HS384", "HS512", "HMACSM3":
			default:
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return []byte(cfg.JWTSecret), nil
		})
		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}

		// Always resolve fresh role and status from DB
		roleID := claims.RoleID
		roleName := claims.RoleName
		userStatus := int16(1)
		if db != nil {
			var user model.User
			if err := db.WithContext(c.Request.Context()).Select("role_id", "status").First(&user, claims.UserID).Error; err == nil {
				roleID = user.RoleID
				userStatus = user.Status
				var role model.Role
				if err := db.WithContext(c.Request.Context()).Select("name").First(&role, roleID).Error; err == nil {
					roleName = role.Name
				}
			}
		}

		// Reject disabled users immediately
		if userStatus == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "account disabled"})
			return
		}

		c.Set("user_id", claims.UserID)
		c.Set("username", claims.Username)
		c.Set("role_id", roleID)
		c.Set("role_name", roleName)
		c.Set("team_id", claims.TeamID)

		// Enforce absolute maximum session lifetime (7 days)
		if claims.IssuedAt != nil && time.Since(claims.IssuedAt.Time) > 7*24*time.Hour {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired, please re-login"})
			return
		}

		// Auto-refresh token if expiring within 5 minutes
		if claims.ExpiresAt != nil && time.Until(claims.ExpiresAt.Time) < 5*time.Minute {
			u := &model.User{ID: claims.UserID, Username: claims.Username, RoleID: roleID}
			if newToken, err := GenerateToken(u, roleName, claims.TeamID, cfg, cp); err == nil {
				c.Header("X-Token-Refresh", newToken)
				c.SetCookie("admin_token", newToken, cfg.TokenExpiry*3600, "/", "", true, true)
			}
		}

		c.Next()
	}
}

// dummyBcryptHash is used to normalize response timing when a username is not found.
const dummyBcryptHash = "$2a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func LoginHandler(userRepo *repository.UserRepo, teamRepo *repository.TeamRepo, roleRepo *repository.RoleRepo, cfg config.AdminConfig, auditSvc *service.AuditService, cp crypto.CryptoProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input struct {
			Username string `json:"username" binding:"required"`
			Password string `json:"password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid request")
			return
		}

		user, err := userRepo.GetByUsername(c.Request.Context(), input.Username)
		if err != nil {
			// Dummy bcrypt to normalize response timing
			bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(input.Password))
			if auditSvc != nil {
				auditSvc.Log(&model.AuditLog{
					UserID:       0,
					Username:     input.Username,
					Action:       "auth:login_failed",
					ResourceType: "auth",
					ResourceID:   "0",
					ResourceName: input.Username,
					Detail:       service.AuditDetail(map[string]any{"method": "password", "reason": "user_not_found"}),
					IPAddress:    c.ClientIP(),
					UserAgent:    c.Request.UserAgent(),
					Status:       "failure",
					CreatedAt:    time.Now().UTC(),
				})
			}
			errorResp(c, http.StatusUnauthorized, ErrInvalidCredentials, "invalid credentials")
			return
		}

		if !verifyPassword(input.Password, user.PasswordHash) {
			if auditSvc != nil {
				auditSvc.Log(&model.AuditLog{
					UserID:       user.ID,
					Username:     user.Username,
					Action:       "auth:login_failed",
					ResourceType: "auth",
					ResourceID:   fmt.Sprintf("%d", user.ID),
					ResourceName: user.Username,
					Detail:       service.AuditDetail(map[string]any{"method": "password", "reason": "invalid_password"}),
					IPAddress:    c.ClientIP(),
					UserAgent:    c.Request.UserAgent(),
					Status:       "failure",
					CreatedAt:    time.Now().UTC(),
				})
			}
			errorResp(c, http.StatusUnauthorized, ErrInvalidCredentials, "invalid credentials")
			return
		}

		// Upgrade legacy SHA-256 hash to bcrypt on first login
		if isLegacyHash(user.PasswordHash) {
			newHash, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcryptCost)
			if err != nil {
				slog.Warn("bcrypt upgrade failed for legacy hash", "error", err)
			} else {
				user.PasswordHash = string(newHash)
				userRepo.Update(c.Request.Context(), user)
			}
		}

		// Resolve role name
		role, _ := roleRepo.GetByID(c.Request.Context(), user.RoleID)
		roleName := ""
		if role != nil {
			roleName = role.Name
		}

		// Resolve team ID for JWT claim
		var teamID int64
		if roleName != model.RoleAdmin {
			teams, _ := teamRepo.ListByUserID(c.Request.Context(), user.ID)
			if len(teams) > 0 {
				teamID = teams[0].ID
			}
		}

		token, err := GenerateToken(user, roleName, teamID, cfg, cp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

		dbActions, _ := roleRepo.GetPermissions(c.Request.Context(), user.RoleID)
		perms := license.EffectiveActions(dbActions)

		// Update last login
		now := time.Now()
		user.LastLoginAt = &now
		userRepo.Update(c.Request.Context(), user)

		// Audit: login success
		if auditSvc != nil {
			auditSvc.Log(&model.AuditLog{
				UserID:       user.ID,
				Username:     user.Username,
				Action:       "auth:login",
				ResourceType: "auth",
				ResourceID:   fmt.Sprintf("%d", user.ID),
				ResourceName: user.Username,
				Detail:       service.AuditDetail(map[string]any{"method": "password", "role": roleName}),
				IPAddress:    c.ClientIP(),
				UserAgent:    c.Request.UserAgent(),
				Status:       "success",
				CreatedAt:    time.Now().UTC(),
			})
		}

		// Set httpOnly cookie
		c.SetCookie("admin_token", token, cfg.TokenExpiry*3600, "/", "", true, true)

		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"token": token,
				"user": gin.H{
					"id":           user.ID,
					"username":     user.Username,
					"display_name": user.DisplayName,
					"role_id":      user.RoleID,
					"role_name":    roleName,
				},
				"permissions": perms,
				"tier":        license.G().CurrentTier(),
			},
		})
	}
}

func verifyPassword(password, hash string) bool {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err == nil {
		return true
	}
	// Fallback to legacy SHA-256
	h := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(hash)) == 1
}

func isLegacyHash(hash string) bool {
	return len(hash) == 64 && !strings.HasPrefix(hash, "$2")
}

// Context helpers for downstream handlers

func GetUserID(c *gin.Context) int64 {
	id, _ := c.Get("user_id")
	if v, ok := id.(int64); ok {
		return v
	}
	return 0
}

func GetRoleID(c *gin.Context) int64 {
	id, _ := c.Get("role_id")
	if v, ok := id.(int64); ok {
		return v
	}
	return 0
}

func GetRoleName(c *gin.Context) string {
	name, _ := c.Get("role_name")
	if v, ok := name.(string); ok {
		return v
	}
	return ""
}

func GetUserRole(c *gin.Context) string {
	return GetRoleName(c)
}

func GetTeamID(c *gin.Context) int64 {
	id, _ := c.Get("team_id")
	if v, ok := id.(int64); ok {
		return v
	}
	return 0
}

func IsAdmin(c *gin.Context) bool {
	return GetRoleName(c) == model.RoleAdmin
}

// isAdminUser checks if a user has the admin role by preloaded Role relation.
func isAdminUser(u *model.User) bool {
	return u.Role.ID != 0 && u.Role.Name == model.RoleAdmin
}

func GetPermissionsHandler(roleRepo *repository.RoleRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		roleID := GetRoleID(c)
		dbActions, err := roleRepo.GetPermissions(c.Request.Context(), roleID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get permissions"})
			return
		}
		perms := license.EffectiveActions(dbActions)
		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"permissions": perms,
				"tier":        license.G().CurrentTier(),
			},
		})
	}
}

func LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.SetCookie("admin_token", "", -1, "/", "", true, true)
		c.JSON(http.StatusOK, gin.H{"message": "logged out"})
	}
}
