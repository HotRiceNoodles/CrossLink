package admin

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// SetSettingsProvider wires the runtime settings provider (called from
// ProvideAdminHandlers; additive so the commercial overlay's SystemHandler
// construction path is unaffected).
func (h *SystemHandler) SetSettingsProvider(p *settings.Provider) {
	h.settings = p
}

// GetConfig handles GET /admin/api/system/config — every managed key with
// its effective value (DB-first) and metadata (section, source, restart
// requirement, sensitivity). Sensitive values are always empty strings;
// their meta carries a "set" bit instead.
func (h *SystemHandler) GetConfig(c *gin.Context) {
	if h.settings == nil {
		internalErr(c, nil, "settings provider not configured")
		return
	}
	snap := h.settings.Get()
	values := snap.APIMap()
	for _, d := range settings.Registry {
		if d.Sensitive {
			values[d.Name] = ""
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"data": gin.H{
			"settings":  values,
			"meta":      snap.MetaMap(),
			"loaded_at": snap.LoadedAt,
		},
	})
}

type configOp struct {
	key    string
	value  string // storage form ("" + delete for removals)
	delete bool
}

// UpdateConfig handles PUT /admin/api/system/config — sparse update.
// JSON null (or an empty sensitive string) deletes the row so the key falls
// back to its yaml/env bootstrap default; omitted keys are unchanged.
// Writes commit in one transaction, then RefreshNow hot-applies (this is the
// hot-reload trigger; pull consumers pick the new snapshot up on their next
// read). Restart-required keys are echoed back so the UI can warn.
func (h *SystemHandler) UpdateConfig(c *gin.Context) {
	if h.settings == nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "not_configured"}))
		}
		internalErr(c, nil, "settings provider not configured")
		return
	}
	var payload map[string]json.RawMessage
	if err := c.ShouldBindJSON(&payload); err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "invalid_request"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if len(payload) == 0 {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "empty_payload"}))
		}
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "no settings supplied")
		return
	}

	var ops []configOp
	var applied, deleted, restartApplied []string
	for key, raw := range payload {
		def, known := settings.RegistryMap[key]
		if !known {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "unknown_config_key"}))
			}
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "unknown config key: "+key)
			return
		}
		rawStr := strings.TrimSpace(string(raw))
		if rawStr == "null" {
			if def.WriteOnce {
				if h.auditSvc != nil {
					h.auditSvc.LogFailure(c, "system:update_config", "setting", key, "", service.AuditDetail(map[string]any{"reason": "write_once_delete"}))
				}
				errorResp(c, http.StatusBadRequest, ErrBillingCurrencyLocked,
					key+" is write-once: it is fixed at first install and cannot be deleted")
				return
			}
			ops = append(ops, configOp{key: key, delete: true})
			continue
		}
		// Empty sensitive string = clear (revert to bootstrap default).
		if def.Sensitive && rawStr == `""` {
			ops = append(ops, configOp{key: key, delete: true})
			continue
		}
		// Write-once keys (billing_currency): the row may be CREATED here (the
		// initial selection) but never changed — a differing value is a 409, a
		// same-value re-PUT is idempotent. Admin-only endpoint, so the
		// check-then-write race between two concurrent first-writes is bounded
		// by the transaction (one insert wins, the other fails on the PK).
		if def.WriteOnce {
			var existing model.SystemSetting
			err := h.db.WithContext(c.Request.Context()).
				Where("system_settings.key = ?", key).First(&existing).Error
			if err == nil && existing.Value != "" {
				value, err := settings.ParseAndValidate(key, raw)
				if err != nil {
					if h.auditSvc != nil {
						h.auditSvc.LogFailure(c, "system:update_config", "setting", key, "", service.AuditDetail(map[string]any{"reason": "invalid_value"}))
					}
					errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
					return
				}
				if value != existing.Value {
					if h.auditSvc != nil {
						h.auditSvc.LogFailure(c, "system:update_config", "setting", key, existing.Value,
							service.AuditDetail(map[string]any{"reason": "write_once_conflict", "requested": value}))
					}
					errorResp(c, http.StatusConflict, ErrBillingCurrencyLocked,
						fmt.Sprintf("%s is locked to %s and cannot be changed (fixed at first install)", key, existing.Value))
					return
				}
				// Same value — nothing to write; report as applied for a
				// truthful response without touching the row.
				applied = append(applied, key)
				continue
			}
		}
		value, err := settings.ParseAndValidate(key, raw)
		if err != nil {
			if h.auditSvc != nil {
				h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "invalid_value"}))
			}
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
			return
		}
		if key == settings.KeyGatewayBaseURL {
			gatewayURL, urlMsg := validateOutboundURL("gateway_base_url", value)
			if urlMsg != "" {
				if h.auditSvc != nil {
					h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "invalid_base_url"}))
				}
				errorResp(c, http.StatusBadRequest, ErrBaseURLInvalid, urlMsg)
				return
			}
			value = gatewayURL
		}
		// Sensitive values are encrypted at rest when a key is active.
		if def.Sensitive {
			stored, encrypted, err := h.settings.EncryptSecret(value)
			if err != nil {
				if h.auditSvc != nil {
					h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "encrypt_failed", "error": err.Error()}))
				}
				internalErr(c, err, "encrypt sensitive setting failed")
				return
			}
			if !encrypted {
				slog.Warn("settings: storing sensitive value as plaintext (no encryption key active)", "key", key)
			}
			value = stored
		}
		ops = append(ops, configOp{key: key, value: value})
	}

	// CORS self-lockout guard: a browser PUT carries Origin; refuse a new
	// allowlist that drops the caller's own origin (lockout prevention).
	if corsRaw, ok := payload[settings.KeyCORSOrigins]; ok && strings.TrimSpace(string(corsRaw)) != "null" {
		if origin := c.GetHeader("Origin"); origin != "" {
			var list []string
			if json.Unmarshal(corsRaw, &list) == nil {
				allowed := settings.NormalizeOrigins(list)
				reqOrigin := strings.ToLower(strings.TrimSpace(origin))
				if !contains(allowed, reqOrigin) {
					if h.auditSvc != nil {
						h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "cors_origin_excluded"}))
					}
					errorResp(c, http.StatusBadRequest, ErrInvalidRequest,
						"cors_allowed_origins would exclude the origin of this request; refusing to apply")
					return
				}
			}
		}
	}

	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for _, op := range ops {
			if op.delete {
				if err := tx.Where("system_settings.key = ?", op.key).Delete(&model.SystemSetting{}).Error; err != nil {
					return err
				}
				deleted = append(deleted, op.key)
				continue
			}
			if err := tx.Save(&model.SystemSetting{Key: op.key, Value: op.value, UpdatedAt: now}).Error; err != nil {
				return err
			}
			applied = append(applied, op.key)
			if settings.RegistryMap[op.key].RestartRequired {
				restartApplied = append(restartApplied, op.key)
			}
		}
		return nil
	})
	if err != nil {
		if h.auditSvc != nil {
			h.auditSvc.LogFailure(c, "system:update_config", "setting", "system", "", service.AuditDetail(map[string]any{"reason": "update_failed", "error": err.Error()}))
		}
		internalErr(c, err, "update config failed")
		return
	}

	// Hot-apply: swap the snapshot so pull consumers read the new values on
	// their next request (and push subscribers — captcha — reconfigure now).
	if err := h.settings.RefreshNow(c.Request.Context()); err != nil {
		slog.Warn("settings refresh after update failed; next poll tick will pick it up", "error", err)
	}

	if h.auditSvc != nil {
		h.auditSvc.LogFromContext(c, "system:update_config", "setting", "system", "",
			service.AuditDetail(map[string]any{"updated_keys": applied, "deleted_keys": deleted}))
	}

	c.JSON(http.StatusOK, gin.H{
		"message":                  "config updated",
		"applied":                  applied,
		"deleted":                  deleted,
		"restart_required_applied": restartApplied,
	})
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// SetFullConfig wires the complete process configuration so the install
// summary can report the install-time parameters (admin config alone lacks
// the master key, billing currency and crypto mode). Additive, like
// SetSettingsProvider: the commercial construction path is unaffected.
func (h *SystemHandler) SetFullConfig(cfg *config.Config) {
	h.fullCfg = cfg
}

// installSummaryItem is one line of the install summary. Secret values are
// NEVER included — only whether they are configured and healthy.
type installSummaryItem struct {
	// Param is the manifest variable the operator set at install time.
	Param string `json:"param"`
	Label string `json:"label"`
	Value string `json:"value"`
	// Secret marks a value that must not be echoed; Value is then empty and
	// Status carries the only information exposed.
	Secret bool `json:"secret"`
	// Status: ok | warn | error | info
	Status string `json:"status"`
	// Locked explains whether/how the parameter is fixed for the deployment.
	Locked string `json:"locked"`
	// Source: where the effective value comes from (env, config, database).
	Source string `json:"source"`
	// Message explains a warn/error in one line (never contains a secret).
	Message string `json:"message,omitempty"`
	// Fix is the suggested remedy for a warn/error item.
	Fix string `json:"fix,omitempty"`
}

// GetInstallSummary handles GET /admin/api/system/install-summary — the
// "installation complete" view: every install-time (A-tier) parameter with
// its effective value, health, and how it is locked. It reuses the preflight
// rules so what the page shows matches what `crosslink -preflight` checks.
//
// Secrets (master key, JWT secret, admin password) are reported as status
// only; their values never leave the process.
func (h *SystemHandler) GetInstallSummary(c *gin.Context) {
	if h.fullCfg == nil {
		internalErr(c, nil, "full configuration not wired")
		return
	}
	report := config.Preflight(h.fullCfg)

	// How each install-time parameter is locked for the deployment.
	locked := map[string]string{
		"CL_BILLING_CURRENCY": "locked once written — change requires redeployment",
		"CL_CRYPTO_MODE":      "deployment decision — change requires redeployment",
		"CL_ENCRYPTION_KEY":   "root key for encrypted secrets — rotate via the key-rotation flow, never by editing",
		"CL_ADMIN_JWT_SECRET": "changing it signs out every admin session",
		"CL_ADMIN_PASSWORD":   "used once to seed the admin user; change it in the UI afterwards",
	}
	source := map[string]string{
		"CL_BILLING_CURRENCY": "environment / config",
		"CL_CRYPTO_MODE":      "environment / config",
		"CL_ENCRYPTION_KEY":   "environment / secret manager",
		"CL_ADMIN_JWT_SECRET": "environment / config",
		"CL_ADMIN_PASSWORD":   "environment / config",
	}

	items := make([]installSummaryItem, 0, len(report.Checks))
	for _, ck := range report.Checks {
		// The database connection is topology, not an install-time decision.
		if ck.Param == "CL_DATABASE_HOST" || ck.Param == "CL_DATABASE_SQLITE_PATH" || ck.Param == "CL_DATABASE_DRIVER" || ck.Param == "" {
			continue
		}
		it := installSummaryItem{
			Param:  ck.Param,
			Label:  ck.Label,
			Status: string(ck.Severity),
			Locked: locked[ck.Param],
			Source: source[ck.Param],
			Fix:    ck.Fix,
		}
		// "info" is not a severity a summary viewer needs to distinguish from ok.
		if it.Status == "info" {
			it.Status = "ok"
		}
		switch ck.Param {
		case "CL_BILLING_CURRENCY":
			// Effective currency, most authoritative first. The settings
			// snapshot resolves DB row > env/yaml declaration, so "locked" is
			// decided by the VALUE'S SOURCE, not by it being non-empty.
			declared := config.NormalizeCurrencyCode(h.fullCfg.Billing.Currency)
			switch {
			case h.settings != nil && h.settings.Get().Source(settings.KeyBillingCurrency) == settings.SourceDB:
				it.Value = h.settings.Get().BillingCurrency
				it.Source = "database (locked)"
			case config.ValidCurrencyCode(declared):
				it.Value = declared + " (declared; locked on first write)"
			default:
				it.Value = "not chosen yet"
			}
		case "CL_CRYPTO_MODE":
			if h.fullCfg.Crypto.Mode == "gm" {
				it.Value = "gm (SM2/SM3/SM4)"
			} else {
				it.Value = "standard (AES)"
			}
		case "CL_ENCRYPTION_KEY", "CL_ADMIN_JWT_SECRET", "CL_ADMIN_PASSWORD":
			it.Secret = true
			if ck.Severity == config.PreflightOK {
				it.Value = "configured"
			}
		}
		items = append(items, it)
	}

	// Deployment shape, best effort: the wizard status lives in system_settings.
	wizard := "not run"
	if h.db != nil {
		var row model.SystemSetting
		if err := h.db.WithContext(c.Request.Context()).
			Where("system_settings.key = ?", "setup_wizard_status").First(&row).Error; err == nil && row.Value != "" {
			wizard = row.Value
		}
	}

	healthy := true
	for _, it := range items {
		if it.Status == "error" {
			healthy = false
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"items":        items,
		"healthy":      healthy,
		"setup_wizard": wizard,
		"currency_source": func() string {
			if h.settings != nil {
				return h.settings.Get().Source(settings.KeyBillingCurrency)
			}
			return "default"
		}(),
	}})
}
