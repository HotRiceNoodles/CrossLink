package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/captcha"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const bcryptCost = 12

// loginResult holds the resolved data needed to complete a login (password or SSO).
type loginResult struct {
	Token       string
	RoleName    string
	TeamID      int64
	OrgID       int64
	OrgName     string
	OrgRole     string
	Permissions []string
	Tier        string
}

// resolveLoginResponse resolves role, team, org, generates a JWT token, and
// builds the common login response. Shared by LoginHandler, ChangeForcedPasswordHandler,
// and SSO callback.
func resolveLoginResponse(c *gin.Context, user *model.User,
	roleRepo *repository.RoleRepo, teamRepo *repository.TeamRepo, orgRepo *repository.OrgRepo,
	cfg config.AdminConfig, cp crypto.CryptoProvider,
) (*loginResult, error) {
	// Resolve role name
	role, _ := roleRepo.GetByID(c.Request.Context(), user.RoleID)
	roleName := ""
	if role != nil {
		roleName = role.Name
	}

	// Resolve team ID for JWT claim (skip for admin)
	var teamID int64
	if roleName != model.RoleAdmin {
		teams, _ := teamRepo.ListByUserID(c.Request.Context(), user.ID)
		if len(teams) > 0 {
			teamID = teams[0].ID
		}
	}

	// Resolve org membership for JWT claim (skip for admin)
	var orgID int64
	var orgRole string
	var orgName string
	if orgRepo != nil && roleName != model.RoleAdmin {
		member, err := orgRepo.GetMemberByUserID(c.Request.Context(), user.ID)
		if err == nil && member != nil {
			orgID = member.OrgID
			orgRole = member.Role
			org, err := orgRepo.GetByID(c.Request.Context(), orgID)
			if err == nil && org != nil {
				orgName = org.DisplayName
			}
		}
	}

	token, err := GenerateToken(user, roleName, teamID, orgID, orgRole, cfg, cp)
	if err != nil {
		return nil, err
	}

	dbActions, _ := roleRepo.GetPermissions(c.Request.Context(), user.RoleID)
	perms := license.EffectiveActions(dbActions)

	return &loginResult{
		Token:       token,
		RoleName:    roleName,
		TeamID:      teamID,
		OrgID:       orgID,
		OrgName:     orgName,
		OrgRole:     orgRole,
		Permissions: perms,
		Tier:        license.G().CurrentTier(),
	}, nil
}

type Claims struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	RoleID   int64  `json:"role_id"`
	RoleName string `json:"role_name"`
	TeamID   int64  `json:"team_id,omitempty"`
	OrgID    int64  `json:"org_id,omitempty"`
	OrgRole  string `json:"org_role,omitempty"`
	jwt.RegisteredClaims
}

func GenerateToken(user *model.User, roleName string, teamID int64, orgID int64, orgRole string, cfg config.AdminConfig, cp crypto.CryptoProvider) (string, error) {
	expiry := time.Duration(cfg.TokenExpiry) * time.Hour
	claims := Claims{
		UserID:   user.ID,
		Username: user.Username,
		RoleID:   user.RoleID,
		RoleName: roleName,
		TeamID:   teamID,
		OrgID:    orgID,
		OrgRole:  orgRole,
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
		forcePasswordChange := false
		passwordHash := ""
		if db != nil {
			var user model.User
			if err := db.WithContext(c.Request.Context()).Select("role_id", "status", "force_password_change", "password_hash").First(&user, claims.UserID).Error; err == nil {
				roleID = user.RoleID
				userStatus = user.Status
				forcePasswordChange = user.ForcePasswordChange
				passwordHash = user.PasswordHash
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
		c.Set("org_id", claims.OrgID)
		c.Set("org_role", claims.OrgRole)

		// Force password change: only allow self-service auth endpoints.
		// SSO users (empty password_hash) skip this check — they cannot change password.
		if forcePasswordChange && passwordHash != "" {
			path := c.Request.URL.Path
			allowed := path == "/admin/api/auth/change-forced-password" ||
				path == "/admin/api/auth/permissions" ||
				path == "/admin/api/user/preferences"
			if !allowed {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error": "password change required",
					"code":  "force_password_change",
				})
				return
			}
		}

		// Enforce absolute maximum session lifetime (7 days)
		if claims.IssuedAt != nil && time.Since(claims.IssuedAt.Time) > 7*24*time.Hour {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "session expired, please re-login"})
			return
		}

		// Auto-refresh token if expiring within 5 minutes
		if claims.ExpiresAt != nil && time.Until(claims.ExpiresAt.Time) < 5*time.Minute {
			u := &model.User{ID: claims.UserID, Username: claims.Username, RoleID: roleID}
			if newToken, err := GenerateToken(u, roleName, claims.TeamID, claims.OrgID, claims.OrgRole, cfg, cp); err == nil {
				c.Header("X-Token-Refresh", newToken)
				c.SetCookie("admin_token", newToken, cfg.TokenExpiry*3600, "/", "", true, true)
			}
		}

		c.Next()
	}
}

// dummyBcryptHash is used to normalize response timing when a username is not found.
const dummyBcryptHash = "$2a$10$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func LoginHandler(userRepo *repository.UserRepo, teamRepo *repository.TeamRepo, roleRepo *repository.RoleRepo, orgRepo *repository.OrgRepo, cfg config.AdminConfig, auditSvc *service.AuditService, cp crypto.CryptoProvider, gate *captcha.Gate) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input struct {
			Username      string         `json:"username" binding:"required"`
			Password      string         `json:"password" binding:"required"`
			CaptchaID     string         `json:"captcha_id"`
			CaptchaAnswer captcha.Answer `json:"captcha_answer"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid request")
			return
		}

		ip := c.ClientIP()

		// Resolve user once (also needed to evaluate device-trust waiver).
		user, _ := userRepo.GetByUsername(c.Request.Context(), input.Username)

		// --- Captcha gate: when enabled, credentials are only checked after
		// the captcha is satisfied (solved this request or trust-waived). An
		// unsatisfied gate returns captcha_required uniformly — never revealing
		// whether the username exists (anti-enumeration). ---
		if gate != nil && gate.Enabled() {
			satisfied := false
			if input.CaptchaID != "" {
				pass, _ := gate.Verify(c.Request.Context(), input.CaptchaID, ip, input.CaptchaAnswer)
				satisfied = pass
			}
			if !satisfied && user != nil {
				trustCookie, _ := c.Cookie(captcha.TrustCookieName)
				satisfied = gate.WaivedByTrust(trustCookie, user.ID, ip)
			}
			if !satisfied {
				errorResp(c, http.StatusBadRequest, "captcha_required", "captcha required")
				return
			}
		}

		if user == nil {
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
					IPAddress:    ip,
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

		result, err := resolveLoginResponse(c, user, roleRepo, teamRepo, orgRepo, cfg, cp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

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
				Detail:       service.AuditDetail(map[string]any{"method": "password", "role": result.RoleName}),
				IPAddress:    c.ClientIP(),
				UserAgent:    c.Request.UserAgent(),
				Status:       "success",
				CreatedAt:    time.Now().UTC(),
			})
		}

		// Set httpOnly cookie
		c.SetCookie("admin_token", result.Token, cfg.TokenExpiry*3600, "/", "", true, true)

		// Set device-memory trust cookie so future logins skip the captcha.
		if gate != nil && gate.Enabled() {
			if trust := gate.IssueTrustCookie(user.ID, ip); trust != "" {
				maxAge := 0
				if d := gate.TrustMaxAgeSeconds(); d > 0 {
					maxAge = d
				}
				c.SetCookie(captcha.TrustCookieName, trust, maxAge, "/", "", true, true)
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"token": result.Token,
				"user": gin.H{
					"id":                    user.ID,
					"username":              user.Username,
					"display_name":          user.DisplayName,
					"role_id":               user.RoleID,
					"role_name":             result.RoleName,
					"org_id":                result.OrgID,
					"org_name":              result.OrgName,
					"org_role":              result.OrgRole,
					"force_password_change": user.ForcePasswordChange,
				},
				"permissions": result.Permissions,
				"tier":        result.Tier,
			},
		})
	}
}

func verifyPassword(password, hash string) bool {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err == nil {
		return true
	}
	// Fallback to legacy SHA-256 — always run bcrypt against dummy first to normalize timing
	bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(password))
	h := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(h[:])), []byte(hash)) == 1
}

func isLegacyHash(hash string) bool {
	return len(hash) == 64 && !strings.HasPrefix(hash, "$2")
}

const passwordCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*"

func GenerateRandomPassword() (string, error) {
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordCharset))))
		if err != nil {
			return "", fmt.Errorf("crypto/rand unavailable: %w", err)
		}
		b[i] = passwordCharset[n.Int64()]
	}
	return string(b), nil
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

func GetOrgID(c *gin.Context) int64 {
	if v, ok := c.Get("org_id"); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

func GetOrgRole(c *gin.Context) string {
	if v, ok := c.Get("org_role"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func IsAdmin(c *gin.Context) bool {
	return GetRoleName(c) == model.RoleAdmin
}

// IsOrgAdmin returns true if the current user has the org_admin role.
func IsOrgAdmin(c *gin.Context) bool {
	return GetRoleName(c) == model.RoleOrgAdmin
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

// CaptchaIssueHandler issues a fresh CAPTCHA challenge. Public (pre-auth).
// Responses:
//   - 404 captcha_disabled      — gate off
//   - 200 {"data":{"trusted":true}} — device already trusted (valid trust
//     cookie); client should NOT render the slider
//   - 200 {"data":<Challenge>}     — challenge issued; client renders slider
func CaptchaIssueHandler(gate *captcha.Gate) gin.HandlerFunc {
	return func(c *gin.Context) {
		if gate == nil || !gate.Enabled() {
			errorResp(c, http.StatusNotFound, "captcha_disabled", "captcha disabled")
			return
		}
		trustCookie, _ := c.Cookie(captcha.TrustCookieName)
		if gate.HasValidTrust(trustCookie, c.ClientIP()) {
			c.JSON(http.StatusOK, gin.H{"data": gin.H{"trusted": true}})
			return
		}
		ch, err := gate.Issue(c.Request.Context(), c.ClientIP(), "login")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to issue captcha"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": ch})
	}
}

func ChangeForcedPasswordHandler(userRepo *repository.UserRepo, roleRepo *repository.RoleRepo, orgRepo *repository.OrgRepo, teamRepo *repository.TeamRepo, cfg config.AdminConfig, auditSvc *service.AuditService, cp crypto.CryptoProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input struct {
			NewPassword     string `json:"new_password" binding:"required"`
			ConfirmPassword string `json:"confirm_password" binding:"required"`
		}
		if err := c.ShouldBindJSON(&input); err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "invalid request")
			return
		}

		if input.NewPassword != input.ConfirmPassword {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "passwords do not match")
			return
		}
		if len(input.NewPassword) < 8 {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "password must be at least 8 characters")
			return
		}

		userID := GetUserID(c)
		user, err := userRepo.GetByID(c.Request.Context(), userID)
		if err != nil {
			errorResp(c, http.StatusNotFound, "user_not_found", "user not found")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(input.NewPassword), bcryptCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to hash password"})
			return
		}

		user.PasswordHash = string(hash)
		user.ForcePasswordChange = false
		if err := userRepo.Update(c.Request.Context(), user); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update password"})
			return
		}

		result, err := resolveLoginResponse(c, user, roleRepo, teamRepo, orgRepo, cfg, cp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
			return
		}

		c.SetCookie("admin_token", result.Token, cfg.TokenExpiry*3600, "/", "", true, true)

		if auditSvc != nil {
			auditSvc.Log(&model.AuditLog{
				UserID:       user.ID,
				Username:     user.Username,
				Action:       "auth:change_forced_password",
				ResourceType: "auth",
				ResourceID:   fmt.Sprintf("%d", user.ID),
				ResourceName: user.Username,
				Detail:       service.AuditDetail(map[string]any{"method": "forced_change"}),
				IPAddress:    c.ClientIP(),
				UserAgent:    c.Request.UserAgent(),
				Status:       "success",
				CreatedAt:    time.Now().UTC(),
			})
		}

		c.JSON(http.StatusOK, gin.H{
			"data": gin.H{
				"token": result.Token,
				"user": gin.H{
					"id":           user.ID,
					"username":     user.Username,
					"display_name": user.DisplayName,
					"role_id":      user.RoleID,
					"role_name":    result.RoleName,
					"org_id":       result.OrgID,
					"org_name":     result.OrgName,
					"org_role":     result.OrgRole,
				},
				"permissions":           result.Permissions,
				"tier":                  result.Tier,
				"force_password_change": false,
			},
		})
	}
}
