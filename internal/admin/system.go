package admin

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/version"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/service"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type SystemHandler struct {
	db         *gorm.DB
	rdb        *redis.Client
	cfg        *config.AdminConfig
	usageSvc   *service.UsageService
	debugStore *debug.Store
	health     *provider.HealthTracker
	budget     *provider.RetryBudget
	auditSvc   *service.AuditService
}

func NewSystemHandler(db *gorm.DB, rdb *redis.Client, cfg config.AdminConfig, usageSvc *service.UsageService, debugStore *debug.Store, health *provider.HealthTracker, budget *provider.RetryBudget, auditSvc *service.AuditService) *SystemHandler {
	return &SystemHandler{db: db, rdb: rdb, cfg: &cfg, usageSvc: usageSvc, debugStore: debugStore, health: health, budget: budget, auditSvc: auditSvc}
}

// ResilienceConfig holds the resilience settings stored in system_settings.
type ResilienceConfig struct {
	CircuitBreakerThreshold int `json:"circuit_breaker_threshold"`
	CircuitBreakerDuration  int `json:"circuit_breaker_duration"` // seconds
	RetryBudgetPerSecond    int `json:"retry_budget_per_second"`
}

// LoadResilienceConfig loads resilience settings from DB, applying defaults for missing keys.
func LoadResilienceConfig(db *gorm.DB) ResilienceConfig {
	rc := ResilienceConfig{
		CircuitBreakerThreshold: 3,
		CircuitBreakerDuration:  60,
		RetryBudgetPerSecond:    100,
	}
	loadInt := func(key string, target *int) {
		var s model.SystemSetting
		if db.Where("key = ?", key).First(&s).Error == nil {
			if v, err := strconv.Atoi(s.Value); err == nil {
				*target = v
			}
		}
	}
	loadInt("circuit_breaker_threshold", &rc.CircuitBreakerThreshold)
	loadInt("circuit_breaker_duration", &rc.CircuitBreakerDuration)
	loadInt("retry_budget_per_second", &rc.RetryBudgetPerSecond)
	return rc
}

// LoadAdminPassword loads the persisted password hash from DB,
// falling back to the config value if none is stored.
func LoadAdminPassword(db *gorm.DB, cfg *config.AdminConfig) {
	var setting model.SystemSetting
	err := db.Where("key = ?", "admin_password_hash").First(&setting).Error
	if err == nil {
		cfg.PasswordHash = setting.Value
	} else {
		hash, _ := bcrypt.GenerateFromPassword([]byte(cfg.Password), bcryptCost)
		cfg.PasswordHash = string(hash)
		if err := db.Save(&model.SystemSetting{Key: "admin_password_hash", Value: cfg.PasswordHash}).Error; err != nil {
			slog.Error("failed to persist initial admin password hash", "error", err)
		}
	}
}

func (h *SystemHandler) Info(c *gin.Context) {
	sqlDB, err := h.db.DB()
	dbStatus := "ok"
	if err != nil || sqlDB.Ping() != nil {
		dbStatus = "error"
	}

	redisStatus := "ok"
	if h.rdb != nil {
		if err := h.rdb.Ping(c.Request.Context()).Err(); err != nil {
			redisStatus = "error"
		}
	} else {
		redisStatus = "error"
	}

	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"db_status":      dbStatus,
			"redis_status":   redisStatus,
			"admin_username": h.cfg.Username,
			"token_expiry":   h.cfg.TokenExpiry,
			"version":        version.Version,
		},
	})
}

func (h *SystemHandler) ChangePassword(c *gin.Context) {
	// Only admin users can change the system admin password.
	// Non-admin users should use ChangeForcedPasswordHandler for self-service.
	if !IsAdmin(c) {
		errorResp(c, http.StatusForbidden, ErrInsufficientPermissions, "insufficient permissions")
		return
	}

	var req struct {
		OldPassword string `json:"old_password" binding:"required"`
		NewPassword string `json:"new_password" binding:"required,min=8,max=128"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		errorResp(c, http.StatusBadRequest, ErrPasswordTooShort, "new password must be at least 8 characters")
		return
	}

	if !verifyPassword(req.OldPassword, h.cfg.PasswordHash) {
		errorResp(c, http.StatusForbidden, ErrIncorrectOldPassword, "old password is incorrect")
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcryptCost)
	if err != nil {
		internalErr(c, err, "hash password failed")
		return
	}
	hashStr := string(newHash)

	err = h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&model.SystemSetting{Key: "admin_password_hash", Value: hashStr}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.User{}).Where("username = ?", h.cfg.Username).Update("password_hash", hashStr).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		internalErr(c, err, "save password failed")
		return
	}
	h.cfg.PasswordHash = hashStr

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "system:password_change", "setting", "admin_password", h.cfg.Username, nil)
	}
	c.JSON(http.StatusOK, gin.H{"message": "password changed"})
}
