package admin

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/crypto"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/secret"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SetupHandler persists first-run setup wizard settings to system_settings.
// All writes go to the KV table (docker-friendly, no yaml round-trip); the
// stats timezone is hot-applied for the Go side, the DB session side picks
// the row up on next restart (two-phase resolution in cmd/server/main.go).
type SetupHandler struct {
	db       *gorm.DB
	cfg      *config.Config
	cp       crypto.CryptoProvider
	encStore *secret.EncryptedDBStore // startup snapshot: nil = process booted keyless
	auditSvc *service.AuditService    // nil in Community
}

func NewSetupHandler(db *gorm.DB, cfg *config.Config, cp crypto.CryptoProvider, encStore *secret.EncryptedDBStore, auditSvc *service.AuditService) *SetupHandler {
	return &SetupHandler{db: db, cfg: cfg, cp: cp, encStore: encStore, auditSvc: auditSvc}
}

type setupApplyRequest struct {
	Timezone              string `json:"timezone"`
	BaseURL               string `json:"base_url"`
	GenerateEncryptionKey bool   `json:"generate_encryption_key"`
	Status                string `json:"status"` // done | pending
}

type setupApplyResult struct {
	Applied            map[string]string `json:"applied"`
	RestartRecommended bool              `json:"restart_recommended"`
}

// Apply upserts all collected settings in one transaction, generates an
// encryption key server-side when requested (refused when a key is already
// active — Pro users rotate via /secrets/rotate-key), and hot-applies the
// stats timezone. Status "pending" keeps the wizard retriable when the
// provider commit failed but base settings should persist.
func (h *SetupHandler) Apply(c *gin.Context) {
	var input setupApplyRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if input.Status != "done" && input.Status != "pending" {
		errorResp(c, http.StatusBadRequest, ErrSetupStatusInvalid, "status must be done or pending")
		return
	}
	if input.Timezone != "" {
		if _, err := time.LoadLocation(input.Timezone); err != nil {
			errorResp(c, http.StatusBadRequest, ErrTimezoneInvalid, "timezone must be a valid IANA name, e.g. Asia/Shanghai")
			return
		}
	}
	if input.BaseURL != "" {
		if !isValidProviderURL(input.BaseURL) {
			errorResp(c, http.StatusBadRequest, ErrBaseURLInvalid, "base_url must start with http:// or https://")
			return
		}
		if u, err := url.Parse(input.BaseURL); err == nil && isInternalHost(u.Hostname()) {
			errorResp(c, http.StatusBadRequest, ErrBaseURLInvalid, "base_url must not point to an internal address")
			return
		}
	}

	// Server-side key generation (never trust a client-supplied key). Refused
	// when any key is already active — rotation is the Pro rotate-key flow.
	newKey := ""
	if input.GenerateEncryptionKey {
		if h.activeKeyExists() {
			errorResp(c, http.StatusConflict, ErrEncryptionKeyActive,
				"an encryption key is already active; use /secrets/rotate-key (Pro) to change it")
			return
		}
		raw := make([]byte, h.cp.CipherKeySize())
		if _, err := rand.Read(raw); err != nil {
			internalErr(c, err, "generate encryption key failed")
			return
		}
		newKey = base64.StdEncoding.EncodeToString(raw)
	}

	applied := map[string]string{}
	now := time.Now().UTC().Format(time.RFC3339)
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if input.Timezone != "" {
			if err := tx.Save(&model.SystemSetting{Key: "stats_timezone", Value: input.Timezone, UpdatedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			applied["stats_timezone"] = input.Timezone
		}
		if input.BaseURL != "" {
			if err := tx.Save(&model.SystemSetting{Key: "gateway_base_url", Value: input.BaseURL, UpdatedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			applied["gateway_base_url"] = input.BaseURL
		}
		if newKey != "" {
			if err := tx.Save(&model.SystemSetting{Key: "encryption_key", Value: newKey, UpdatedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			applied["encryption_key"] = "generated"
		}
		if err := tx.Save(&model.SystemSetting{Key: "setup_wizard_status", Value: input.Status, UpdatedAt: time.Now().UTC()}).Error; err != nil {
			return err
		}
		applied["setup_wizard_status"] = input.Status
		if input.Status == "done" {
			if err := tx.Save(&model.SystemSetting{Key: "setup_wizard_completed_at", Value: now, UpdatedAt: time.Now().UTC()}).Error; err != nil {
				return err
			}
			applied["setup_wizard_completed_at"] = now
		}
		return nil
	})
	if err != nil {
		internalErr(c, err, "apply setup settings failed")
		return
	}

	// Hot-apply the Go-side stats timezone immediately; the DB session
	// timezone follows on next restart via the two-phase resolution.
	if input.Timezone != "" {
		SetStatsTimezone(input.Timezone)
	}

	// A freshly generated key needs the watcher (or, pre-Task-5, a restart)
	// to become the active store — surface that to the wizard UI.
	restartRecommended := newKey != "" && h.encStore == nil

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "system:setup_apply", "setting", "setup", "wizard", service.AuditDetail(applied))
	}
	c.JSON(http.StatusOK, gin.H{"data": setupApplyResult{Applied: applied, RestartRecommended: restartRecommended}})
}

// Skip records that the wizard was dismissed without completion.
// setup_needed turns false permanently; the readiness report keeps a
// reminder item and the dashboard banner stays until the wizard completes.
func (h *SetupHandler) Skip(c *gin.Context) {
	if err := h.db.WithContext(c.Request.Context()).Save(&model.SystemSetting{Key: "setup_wizard_status", Value: "skipped", UpdatedAt: time.Now().UTC()}).Error; err != nil {
		internalErr(c, err, "record setup skip failed")
		return
	}
	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "system:setup_skip", "setting", "setup_wizard_status", "wizard", nil)
	}
	c.JSON(http.StatusOK, gin.H{"message": "setup wizard skipped"})
}

func (h *SetupHandler) activeKeyExists() bool {
	if h.cfg.SecretManager.EncryptionKey != "" {
		return true
	}
	var row model.SystemSetting
	return h.db.Where("key = ?", "encryption_key").First(&row).Error == nil && row.Value != ""
}
