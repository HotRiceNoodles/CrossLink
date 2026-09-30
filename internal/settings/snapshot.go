package settings

import (
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/secret"
)

// Snapshot is an immutable view of every migrated config section, resolved
// as system_settings row > yaml/env baseline. Built by the Provider on every
// refresh; consumers read it per request / per send.
type Snapshot struct {
	SMTP           config.SMTPConfig
	GatewayBaseURL string
	CORSAllowed    []string
	RateLimit      config.RateLimitConfig
	GuardrailAlert config.GuardrailAlertConfig
	IPBinding      config.IPBindingConfig
	DataLens       config.DataLensConfig
	MCP            config.MCPConfig
	Captcha        config.CaptchaConfig

	// Sources maps every registry key to "db" (row applied), "default"
	// (yaml/env value in effect), or "db-error" (row present but not
	// applicable — e.g. an encrypted smtp_password with no active key).
	Sources map[string]string
	// SensitiveSet reports, for sensitive keys only, whether a value is
	// configured (DB row present or yaml/env default non-empty). The value
	// itself never leaves the process.
	SensitiveSet map[string]bool
	// LoadedAt is the snapshot build time (echoed by GET /system/config).
	LoadedAt time.Time
}

// Source returns the provenance of the given key ("default" when unknown).
func (s *Snapshot) Source(key string) string {
	if s.Sources == nil {
		return SourceDefault
	}
	if v, ok := s.Sources[key]; ok {
		return v
	}
	return SourceDefault
}

// Source values for Snapshot.Sources.
const (
	SourceDB      = "db"
	SourceDefault = "default"
	SourceDBError = "db-error"
)

// decryptFunc decrypts enc:// and enc2:// values; nil = no active key.
type decryptFunc func(string) (string, error)

// buildSnapshot overlays DB values onto a copy of the yaml/env baseline.
// values holds raw system_settings rows for managed keys (missing keys
// absent). Unparseable rows are skipped with the source marked "db-error"
// — a bad row must never take the process down.
func buildSnapshot(baseline *config.Config, values map[string]string, decrypt decryptFunc) *Snapshot {
	snap := &Snapshot{
		SMTP:           baseline.SMTP,
		GatewayBaseURL: baseline.Gateway.BaseURL,
		CORSAllowed:    baseline.CORS.AllowedOrigins,
		RateLimit:      baseline.RateLimit,
		GuardrailAlert: baseline.GuardrailAlert,
		IPBinding:      baseline.IPBinding,
		DataLens:       baseline.DataLens,
		MCP:            baseline.MCP,
		Captcha:        baseline.Captcha,
		Sources:        make(map[string]string, len(Registry)),
		SensitiveSet:   make(map[string]bool),
	}
	for _, d := range Registry {
		snap.Sources[d.Name] = SourceDefault
	}
	// Sensitive "configured" bits: yaml/env default non-empty, or a DB row.
	snap.SensitiveSet[KeySMTPPassword] = baseline.SMTP.Password != "" || values[KeySMTPPassword] != ""

	str := func(key string, target *string) bool {
		v, ok := values[key]
		if !ok {
			return false
		}
		*target = v
		return true
	}
	boolean := func(key string, target *bool) bool {
		v, ok := values[key]
		if !ok {
			return false
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			snap.Sources[key] = SourceDBError
			return false
		}
		*target = b
		return true
	}
	integer := func(key string, target *int) bool {
		v, ok := values[key]
		if !ok {
			return false
		}
		i, err := strconv.Atoi(v)
		if err != nil {
			snap.Sources[key] = SourceDBError
			return false
		}
		*target = i
		return true
	}
	float := func(key string, target *float64) bool {
		v, ok := values[key]
		if !ok {
			return false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			snap.Sources[key] = SourceDBError
			return false
		}
		*target = f
		return true
	}
	strSlice := func(key string, target *[]string) bool {
		v, ok := values[key]
		if !ok {
			return false
		}
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err != nil {
			snap.Sources[key] = SourceDBError
			return false
		}
		*target = list
		return true
	}
	seconds := func(key string, target *time.Duration) bool {
		i := 0
		if !integer(key, &i) {
			return false
		}
		*target = time.Duration(i) * time.Second
		return true
	}
	apply := func(key string, ok bool) {
		if ok {
			snap.Sources[key] = SourceDB
		}
	}

	// --- smtp ---
	apply(KeySMTPHost, str(KeySMTPHost, &snap.SMTP.Host))
	apply(KeySMTPPort, integer(KeySMTPPort, &snap.SMTP.Port))
	apply(KeySMTPUsername, str(KeySMTPUsername, &snap.SMTP.Username))
	apply(KeySMTPFrom, str(KeySMTPFrom, &snap.SMTP.From))
	if raw, ok := values[KeySMTPPassword]; ok {
		if !secret.IsEncryptedRef(raw) {
			// Plaintext row (written while keyless). Same fallback contract
			// as provider secrets: usable as-is; re-encrypted opportunistically
			// on the next PUT once a key is active.
			snap.SMTP.Password = raw
			snap.Sources[KeySMTPPassword] = SourceDB
		} else if decrypt != nil {
			if plain, err := decrypt(raw); err == nil {
				snap.SMTP.Password = plain
				snap.Sources[KeySMTPPassword] = SourceDB
			} else {
				slog.Warn("settings: smtp_password row undecryptable, keeping baseline", "error", err)
				snap.Sources[KeySMTPPassword] = SourceDBError
			}
		} else {
			// Encrypted row but the process has no active key — value stays
			// the yaml baseline; surfaced via Sources for the doctor/UI.
			slog.Warn("settings: smtp_password row is encrypted but no key is active")
			snap.Sources[KeySMTPPassword] = SourceDBError
		}
	}

	// --- gateway ---
	apply(KeyGatewayBaseURL, str(KeyGatewayBaseURL, &snap.GatewayBaseURL))

	// --- cors ---
	apply(KeyCORSOrigins, strSlice(KeyCORSOrigins, &snap.CORSAllowed))

	// --- rate_limit ---
	apply(KeyRateLimitRPM, integer(KeyRateLimitRPM, &snap.RateLimit.RPM))
	apply(KeyRateLimitTPM, integer(KeyRateLimitTPM, &snap.RateLimit.TPM))
	apply(KeyRateLimitReserve, integer(KeyRateLimitReserve, &snap.RateLimit.Reservation))
	apply(KeyRateLimitFail, boolean(KeyRateLimitFail, &snap.RateLimit.FailClosed))

	// --- captcha ---
	apply(KeyCaptchaEnabled, boolean(KeyCaptchaEnabled, &snap.Captcha.Enabled))
	apply(KeyCaptchaProvider, str(KeyCaptchaProvider, &snap.Captcha.Provider))
	apply(KeyCaptchaTrustDays, integer(KeyCaptchaTrustDays, &snap.Captcha.TrustDays))
	apply(KeyCaptchaIPMask, integer(KeyCaptchaIPMask, &snap.Captcha.TrustIPMask))
	apply(KeyCaptchaFailOpen, boolean(KeyCaptchaFailOpen, &snap.Captcha.RedisFailOpen))
	apply(KeySliderTolerance, float(KeySliderTolerance, &snap.Captcha.Slider.TolerancePx))
	apply(KeySliderMinPoints, integer(KeySliderMinPoints, &snap.Captcha.Slider.MinPoints))
	apply(KeySliderBGWidth, integer(KeySliderBGWidth, &snap.Captcha.Slider.BGWidth))
	apply(KeySliderBGHeight, integer(KeySliderBGHeight, &snap.Captcha.Slider.BGHeight))
	apply(KeySliderPieceSize, integer(KeySliderPieceSize, &snap.Captcha.Slider.PieceSize))

	// --- datalens ---
	apply(KeyDataLensEnabled, boolean(KeyDataLensEnabled, &snap.DataLens.Enabled))
	apply(KeyDataLensInterval, str(KeyDataLensInterval, &snap.DataLens.Agg.Interval))
	apply(KeyDataLensLookback, str(KeyDataLensLookback, &snap.DataLens.Agg.Lookback))
	apply(KeyDataLensBackfill, integer(KeyDataLensBackfill, &snap.DataLens.Agg.BackfillDays))
	apply(KeyDataLensRawLogs, integer(KeyDataLensRawLogs, &snap.DataLens.Retention.RawLogsDays))
	apply(KeyDataLensHourly, integer(KeyDataLensHourly, &snap.DataLens.Retention.HourlyDays))
	apply(KeyDataLensDaily, integer(KeyDataLensDaily, &snap.DataLens.Retention.DailyDays))
	apply(KeyDataLensFromName, str(KeyDataLensFromName, &snap.DataLens.FromName))
	apply(KeyDataLensFromAddr, str(KeyDataLensFromAddr, &snap.DataLens.FromAddr))

	// --- mcp ---
	apply(KeyMCPEnabled, boolean(KeyMCPEnabled, &snap.MCP.Enabled))
	apply(KeyMCPMaxServers, integer(KeyMCPMaxServers, &snap.MCP.MaxServers))
	apply(KeyMCPToolCacheTTL, seconds(KeyMCPToolCacheTTL, &snap.MCP.ToolCacheTTL))
	apply(KeyMCPRequestTTL, seconds(KeyMCPRequestTTL, &snap.MCP.RequestTimeout))
	apply(KeyMCPHealthCheck, seconds(KeyMCPHealthCheck, &snap.MCP.HealthCheckInterval))
	apply(KeyMCPMaxIdleConns, integer(KeyMCPMaxIdleConns, &snap.MCP.HTTPMaxIdleConns))
	apply(KeyMCPRLEnabled, boolean(KeyMCPRLEnabled, &snap.MCP.RateLimitEnabled))
	apply(KeyMCPRLDefaultRPM, integer(KeyMCPRLDefaultRPM, &snap.MCP.RateLimitDefaultRPM))
	apply(KeyMCPLogRetention, integer(KeyMCPLogRetention, &snap.MCP.LogRetentionDays))

	// --- guardrail_alert ---
	apply(KeyGAEnabled, boolean(KeyGAEnabled, &snap.GuardrailAlert.Enabled))
	apply(KeyGAConcurrency, integer(KeyGAConcurrency, &snap.GuardrailAlert.Concurrency))
	apply(KeyGAContentPreview, boolean(KeyGAContentPreview, &snap.GuardrailAlert.ContentPreview))
	apply(KeyGAPreviewLen, integer(KeyGAPreviewLen, &snap.GuardrailAlert.ContentPreviewLen))

	// --- ip_binding ---
	apply(KeyIPBCooldown, integer(KeyIPBCooldown, &snap.IPBinding.NotifyCooldownSeconds))

	snap.LoadedAt = time.Now().UTC()
	return snap
}

// NormalizeOrigins lowercases and trims origins for CORS map building.
func NormalizeOrigins(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		t := strings.ToLower(strings.TrimSpace(o))
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
