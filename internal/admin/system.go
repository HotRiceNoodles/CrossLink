package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/debug"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/version"
	"github.com/gin-gonic/gin"
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
	CircuitBreakerThreshold int `json:"circuit_breaker_threshold"` // = transient threshold
	CircuitBreakerDuration  int `json:"circuit_breaker_duration"`  // = transient cooldown (seconds)
	RetryBudgetPerSecond    int `json:"retry_budget_per_second"`
	PersistentCooldown      int `json:"persistent_cooldown"` // seconds; quota/billing failures
	RetryAfterMin           int `json:"retry_after_min"`     // seconds; transient Retry-After clamp lower bound
	RetryAfterMax           int `json:"retry_after_max"`     // seconds; transient Retry-After clamp upper bound
}

// LoadResilienceConfig loads resilience settings from DB, applying defaults for missing keys.
func LoadResilienceConfig(db *gorm.DB) ResilienceConfig {
	rc := ResilienceConfig{
		CircuitBreakerThreshold: 3,
		CircuitBreakerDuration:  60,
		RetryBudgetPerSecond:    100,
		PersistentCooldown:      1800,
		RetryAfterMin:           5,
		RetryAfterMax:           300,
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
	loadInt("persistent_cooldown", &rc.PersistentCooldown)
	loadInt("retry_after_min", &rc.RetryAfterMin)
	loadInt("retry_after_max", &rc.RetryAfterMax)
	return rc
}

// RunResilienceRefreshLoop periodically reloads resilience settings from the DB and
// applies them to the HealthTracker, so cooldowns/thresholds can be tuned at runtime
// without a restart. Runs until ctx is cancelled. (HealthTracker.UpdateConfig and
// setters are no-ops-safe to call concurrently.)
func RunResilienceRefreshLoop(ctx context.Context, db *gorm.DB, health *provider.HealthTracker, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	apply := func() {
		rc := LoadResilienceConfig(db)
		health.UpdateConfig(rc.CircuitBreakerThreshold, time.Duration(rc.CircuitBreakerDuration)*time.Second)
		health.SetPersistentCooldown(time.Duration(rc.PersistentCooldown) * time.Second)
		health.SetRetryAfterBounds(time.Duration(rc.RetryAfterMin)*time.Second, time.Duration(rc.RetryAfterMax)*time.Second)
	}
	apply()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			apply()
		}
	}
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
