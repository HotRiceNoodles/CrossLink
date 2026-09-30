// Package settings provides the runtime (DB-backed) operational config
// provider. Precedence per key: system_settings row > yaml/env value loaded
// by internal/config. With no DB rows the snapshot equals the yaml/env
// values — behavior is byte-identical to the pre-P2 world (hard compat
// contract, covered by tests).
//
// All migrated sections live here: smtp, gateway.base_url, cors,
// rate_limit defaults, captcha, datalens, mcp, guardrail_alert, ip_binding.
// cache / stats_timezone / encryption_key keep their existing separate
// mechanisms.
package settings

// KeyDef describes one system_settings key managed by this package.
type KeyDef struct {
	Name string
	// Type controls parsing/stringifying and the PUT payload shape:
	// string | int | float | bool | string[] (JSON array) | duration_seconds (stored as int seconds).
	Type string
	// Section groups the key in the admin UI and GET /system/config meta.
	Section string
	// Sensitive: never returned by the API, encrypted at rest when a key is
	// active (smtp_password).
	Sensitive bool
	// RestartRequired: the consumer is boot-pinned (ticker / semaphore /
	// route mounting / transport pool); changes need a restart. Honest label,
	// surfaced in GET meta and the Settings UI.
	RestartRequired bool
	// Min/Max bound int/float values (Min set means >= Min; Max > Min means <= Max).
	hasRange bool
	Min      float64
	Max      float64
	// Validate optionally checks non-numeric shapes (e.g. duration strings,
	// origins). Receives the parsed Go value (string, int, float64, bool,
	// []string).
	Validate func(v any) error
}

// Key types.
const (
	TypeString           = "string"
	TypeInt              = "int"
	TypeFloat            = "float"
	TypeBool             = "bool"
	TypeStringSlice      = "string[]"
	TypeDurationSeconds  = "duration_seconds"
)

// Key name constants — shared by the provider, the admin handler, and tests.
const (
	KeySMTPHost         = "smtp_host"
	KeySMTPPort         = "smtp_port"
	KeySMTPUsername     = "smtp_username"
	KeySMTPPassword     = "smtp_password"
	KeySMTPFrom         = "smtp_from"
	KeyGatewayBaseURL   = "gateway_base_url" // shared with the setup wizard (pilot)
	KeyCORSOrigins      = "cors_allowed_origins"
	KeyRateLimitRPM     = "rate_limit_rpm"
	KeyRateLimitTPM     = "rate_limit_tpm"
	KeyRateLimitReserve = "rate_limit_tpm_reservation"
	KeyRateLimitFail    = "rate_limit_fail_closed"
	KeyCaptchaEnabled   = "captcha_enabled"
	KeyCaptchaProvider  = "captcha_provider"
	KeyCaptchaTrustDays = "captcha_trust_days"
	KeyCaptchaIPMask    = "captcha_trust_ip_mask"
	KeyCaptchaFailOpen  = "captcha_redis_fail_open"
	KeySliderTolerance  = "captcha_slider_tolerance_px"
	KeySliderMinPoints  = "captcha_slider_min_points"
	KeySliderBGWidth    = "captcha_slider_bg_width"
	KeySliderBGHeight   = "captcha_slider_bg_height"
	KeySliderPieceSize  = "captcha_slider_piece_size"
	KeyDataLensEnabled  = "datalens_enabled"
	KeyDataLensInterval = "datalens_agg_interval"
	KeyDataLensLookback = "datalens_agg_lookback"
	KeyDataLensBackfill = "datalens_agg_backfill_days"
	KeyDataLensRawLogs  = "datalens_retention_raw_logs_days"
	KeyDataLensHourly   = "datalens_retention_hourly_days"
	KeyDataLensDaily    = "datalens_retention_daily_days"
	KeyDataLensFromName = "datalens_from_name"
	KeyDataLensFromAddr = "datalens_from_addr"
	KeyMCPEnabled       = "mcp_enabled"
	KeyMCPMaxServers    = "mcp_max_servers"
	KeyMCPToolCacheTTL  = "mcp_tool_cache_ttl"
	KeyMCPRequestTTL    = "mcp_request_timeout"
	KeyMCPHealthCheck   = "mcp_health_check_interval"
	KeyMCPMaxIdleConns  = "mcp_http_max_idle_conns"
	KeyMCPRLEnabled     = "mcp_rate_limit_enabled"
	KeyMCPRLDefaultRPM  = "mcp_rate_limit_default_rpm"
	KeyMCPLogRetention  = "mcp_log_retention_days"
	KeyGAEnabled        = "guardrail_alert_enabled"
	KeyGAConcurrency    = "guardrail_alert_concurrency"
	KeyGAContentPreview = "guardrail_alert_content_preview"
	KeyGAPreviewLen     = "guardrail_alert_content_preview_len"
	KeyIPBCooldown      = "ip_binding_notify_cooldown_seconds"
)

// section names (meta grouping + UI).
const (
	SectionSMTP           = "smtp"
	SectionGateway        = "gateway"
	SectionCORS           = "cors"
	SectionRateLimit      = "rate_limit"
	SectionCaptcha        = "captcha"
	SectionDataLens       = "datalens"
	SectionMCP            = "mcp"
	SectionGuardrailAlert = "guardrail_alert"
	SectionIPBinding      = "ip_binding"
)

func k(name, typ, section string) KeyDef {
	return KeyDef{Name: name, Type: typ, Section: section}
}

func kRange(name, typ, section string, min, max float64) KeyDef {
	d := k(name, typ, section)
	d.hasRange = true
	d.Min = min
	d.Max = max
	return d
}

// restart marks a boot-pinned key (changes take effect after restart).
func restart(d KeyDef) KeyDef {
	d.RestartRequired = true
	return d
}

// sensitive marks a value never returned by the API.
func sensitive(d KeyDef) KeyDef {
	d.Sensitive = true
	return d
}

// Registry is the ordered list of every managed key. GET /system/config meta
// and the PUT validator are derived from it.
var Registry = []KeyDef{
	// --- smtp (consumed by the commercial overlay email senders) ---
	k(KeySMTPHost, TypeString, SectionSMTP),
	kRange(KeySMTPPort, TypeInt, SectionSMTP, 1, 65535),
	k(KeySMTPUsername, TypeString, SectionSMTP),
	sensitive(k(KeySMTPPassword, TypeString, SectionSMTP)),
	k(KeySMTPFrom, TypeString, SectionSMTP),

	// --- gateway ---
	k(KeyGatewayBaseURL, TypeString, SectionGateway),

	// --- cors ---
	k(KeyCORSOrigins, TypeStringSlice, SectionCORS),

	// --- rate_limit defaults ---
	kRange(KeyRateLimitRPM, TypeInt, SectionRateLimit, 0, 0),
	kRange(KeyRateLimitTPM, TypeInt, SectionRateLimit, 0, 0),
	kRange(KeyRateLimitReserve, TypeInt, SectionRateLimit, 0, 0),
	k(KeyRateLimitFail, TypeBool, SectionRateLimit),

	// --- captcha (provider switch needs a restart: cloud providers are
	// overlay + boot fallback) ---
	k(KeyCaptchaEnabled, TypeBool, SectionCaptcha),
	restart(k(KeyCaptchaProvider, TypeString, SectionCaptcha)),
	kRange(KeyCaptchaTrustDays, TypeInt, SectionCaptcha, 0, 365),
	kRange(KeyCaptchaIPMask, TypeInt, SectionCaptcha, 0, 128),
	k(KeyCaptchaFailOpen, TypeBool, SectionCaptcha),
	kRange(KeySliderTolerance, TypeFloat, SectionCaptcha, 0, 0),
	kRange(KeySliderMinPoints, TypeInt, SectionCaptcha, 0, 0),
	kRange(KeySliderBGWidth, TypeInt, SectionCaptcha, 0, 0),
	kRange(KeySliderBGHeight, TypeInt, SectionCaptcha, 0, 0),
	kRange(KeySliderPieceSize, TypeInt, SectionCaptcha, 0, 0),

	// --- datalens (aggregation loop + retention) ---
	restart(k(KeyDataLensEnabled, TypeBool, SectionDataLens)),
	restart(k(KeyDataLensInterval, TypeString, SectionDataLens)),
	restart(k(KeyDataLensLookback, TypeString, SectionDataLens)),
	restart(kRange(KeyDataLensBackfill, TypeInt, SectionDataLens, 0, 0)),
	kRange(KeyDataLensRawLogs, TypeInt, SectionDataLens, 0, 0),
	kRange(KeyDataLensHourly, TypeInt, SectionDataLens, 0, 0),
	kRange(KeyDataLensDaily, TypeInt, SectionDataLens, 0, 0),
	k(KeyDataLensFromName, TypeString, SectionDataLens),
	k(KeyDataLensFromAddr, TypeString, SectionDataLens),

	// --- mcp (hot subset: max_servers, tool ttl, request timeout;
	// the rest is baked into tickers/pools/route mounting) ---
	restart(k(KeyMCPEnabled, TypeBool, SectionMCP)),
	kRange(KeyMCPMaxServers, TypeInt, SectionMCP, 0, 0),
	k(KeyMCPToolCacheTTL, TypeDurationSeconds, SectionMCP),
	k(KeyMCPRequestTTL, TypeDurationSeconds, SectionMCP),
	restart(k(KeyMCPHealthCheck, TypeDurationSeconds, SectionMCP)),
	restart(kRange(KeyMCPMaxIdleConns, TypeInt, SectionMCP, 1, 0)),
	restart(k(KeyMCPRLEnabled, TypeBool, SectionMCP)),
	restart(kRange(KeyMCPRLDefaultRPM, TypeInt, SectionMCP, 0, 0)),
	restart(kRange(KeyMCPLogRetention, TypeInt, SectionMCP, 0, 0)),

	// --- guardrail_alert (overlay alert pipeline) ---
	k(KeyGAEnabled, TypeBool, SectionGuardrailAlert),
	restart(kRange(KeyGAConcurrency, TypeInt, SectionGuardrailAlert, 1, 64)),
	k(KeyGAContentPreview, TypeBool, SectionGuardrailAlert),
	kRange(KeyGAPreviewLen, TypeInt, SectionGuardrailAlert, 0, 2000),

	// --- ip_binding (overlay email alerts) ---
	kRange(KeyIPBCooldown, TypeInt, SectionIPBinding, 0, 86400),
}

// RegistryMap indexes Registry by key name.
var RegistryMap = func() map[string]KeyDef {
	m := make(map[string]KeyDef, len(Registry))
	for _, d := range Registry {
		m[d.Name] = d
	}
	return m
}()

// KeyNames returns every managed key (for the provider's WHERE IN query).
func KeyNames() []string {
	names := make([]string, len(Registry))
	for i, d := range Registry {
		names[i] = d.Name
	}
	return names
}
