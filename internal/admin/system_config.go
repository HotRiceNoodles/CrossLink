package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	key   string
	value string // storage form ("" + delete for removals)
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
		internalErr(c, nil, "settings provider not configured")
		return
	}
	var payload map[string]json.RawMessage
	if err := c.ShouldBindJSON(&payload); err != nil {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
		return
	}
	if len(payload) == 0 {
		errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "no settings supplied")
		return
	}

	var ops []configOp
	for key, raw := range payload {
		def, known := settings.RegistryMap[key]
		if !known {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, "unknown config key: "+key)
			return
		}
		rawStr := strings.TrimSpace(string(raw))
		if rawStr == "null" {
			ops = append(ops, configOp{key: key, delete: true})
			continue
		}
		// Empty sensitive string = clear (revert to bootstrap default).
		if def.Sensitive && rawStr == `""` {
			ops = append(ops, configOp{key: key, delete: true})
			continue
		}
		value, err := settings.ParseAndValidate(key, raw)
		if err != nil {
			errorResp(c, http.StatusBadRequest, ErrInvalidRequest, err.Error())
			return
		}
		if key == settings.KeyGatewayBaseURL {
			if !isValidProviderURL(value) {
				errorResp(c, http.StatusBadRequest, ErrBaseURLInvalid, "gateway_base_url must start with http:// or https://")
				return
			}
			if u, err := url.Parse(value); err == nil && isInternalHost(u.Hostname()) {
				errorResp(c, http.StatusBadRequest, ErrBaseURLInvalid, "gateway_base_url must not point to an internal address")
				return
			}
		}
		// Sensitive values are encrypted at rest when a key is active.
		if def.Sensitive {
			stored, encrypted, err := h.settings.EncryptSecret(value)
			if err != nil {
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
					errorResp(c, http.StatusBadRequest, ErrInvalidRequest,
						"cors_allowed_origins would exclude the origin of this request; refusing to apply")
					return
				}
			}
		}
	}

	var applied, deleted, restartApplied []string
	err := h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		for _, op := range ops {
			if op.delete {
				if err := tx.Where("key = ?", op.key).Delete(&model.SystemSetting{}).Error; err != nil {
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
