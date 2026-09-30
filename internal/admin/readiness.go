package admin

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/crosslink/internal/config"
	"github.com/crosslink/internal/license"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/settings"
	"github.com/crosslink/internal/secret"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Severity levels for readiness checks, most to least urgent.
const (
	SeverityDanger = "danger"
	SeverityWarn   = "warn"
	SeverityInfo   = "info"
	SeverityOK     = "ok"
)

// Fix actions the frontend maps to navigation. Empty = no action offered.
const (
	FixOpenWizard   = "open_wizard"   // dispatch reopen-onboarding (setup mode)
	FixOpenSettings = "open_settings" // router.push('/settings#doctor')
	FixDocs         = "docs"          // documentation hint only
)

var severityOrder = map[string]int{SeverityDanger: 0, SeverityWarn: 1, SeverityInfo: 2, SeverityOK: 3}

// ReadinessCheck is one entry of the readiness report. Title/detail/hint are
// i18n keys — the backend never localizes; DetailParams carries counts/values
// for frontend interpolation.
type ReadinessCheck struct {
	ID           string         `json:"id"`
	Severity     string         `json:"severity"`
	TitleKey     string         `json:"title_key"`
	DetailKey    string         `json:"detail_key"`
	DetailParams map[string]any `json:"detail_params,omitempty"`
	FixHintKey   string         `json:"fix_hint_key,omitempty"`
	FixAction    string         `json:"fix_action,omitempty"`
}

type ReadinessSummary struct {
	Danger int `json:"danger"`
	Warn   int `json:"warn"`
	Info   int `json:"info"`
	OK     int `json:"ok"`
}

type ReadinessReport struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Tier        string           `json:"tier"`
	SetupNeeded bool             `json:"setup_needed"`
	Summary     ReadinessSummary `json:"summary"`
	Checks      []ReadinessCheck `json:"checks"`
}

// readinessInputs gathers all DB/config state once so each check is a pure
// function of the inputs (individually testable, no repeated queries).
type readinessInputs struct {
	settings         map[string]string // system_settings key → value
	providerCount    int64
	apiKeyCount      int64
	plaintextSecrets int // providers with plaintext secrets at rest
}

// ReadinessChecker evaluates configuration/deployment health for the config
// doctor (P0) and computes the setup-wizard trigger flag (P1).
type ReadinessChecker struct {
	db  *gorm.DB
	cfg *config.Config
	// settings, when wired, supplies effective (DB-first) values for the
	// SMTP/CORS checks. nil ⇒ static config reads.
	settings *settings.Provider
}

func NewReadinessChecker(db *gorm.DB, cfg *config.Config) *ReadinessChecker {
	return &ReadinessChecker{db: db, cfg: cfg}
}

// SetSettingsProvider wires the runtime settings source (optional).
func (r *ReadinessChecker) SetSettingsProvider(p *settings.Provider) {
	r.settings = p
}

// effSMTPHost / effCORSOrigins resolve effective values (DB-first).
func (r *ReadinessChecker) effSMTPHost() string {
	if r.settings != nil {
		return r.settings.SMTP().Host
	}
	return r.cfg.SMTP.Host
}

func (r *ReadinessChecker) effCORSOrigins() []string {
	if r.settings != nil {
		return r.settings.CORSAllowed()
	}
	return r.cfg.CORS.AllowedOrigins
}

func (r *ReadinessChecker) gather(ctx context.Context) (*readinessInputs, error) {
	inp := &readinessInputs{settings: map[string]string{}}
	var rows []model.SystemSetting
	if err := r.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		inp.settings[row.Key] = row.Value
	}
	if err := r.db.WithContext(ctx).Model(&model.Provider{}).Count(&inp.providerCount).Error; err != nil {
		return nil, err
	}
	if err := r.db.WithContext(ctx).Model(&model.APIKey{}).Count(&inp.apiKeyCount).Error; err != nil {
		return nil, err
	}
	// Table scan is cheap (name/api_key/extra_config only) and reuses the same
	// plaintext predicates as the secret migration.
	plaintext, err := secret.CountPlaintextProviderSecrets(r.db.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	inp.plaintextSecrets = plaintext
	return inp, nil
}

// encryptionActive mirrors secret.InitActiveEncryption precedence: the DB row
// wins over secret_manager.encryption_key; both empty = no encryption.
func (r *ReadinessChecker) encryptionActive(inp *readinessInputs) bool {
	return r.cfg.SecretManager.EncryptionKey != "" || inp.settings["encryption_key"] != ""
}

// SetupNeeded reports whether the first-run setup wizard should be forced:
// the setup_wizard_status row is absent AND no provider exists yet. Existing
// installs that finished the (pre-setup-mode) onboarding have providers, so
// they are never re-forced. After skip/done the row exists and this stays
// false permanently.
func (r *ReadinessChecker) SetupNeeded(ctx context.Context) bool {
	inp, err := r.gather(ctx)
	if err != nil {
		return false // never block login on a readiness error
	}
	_, hasStatus := inp.settings["setup_wizard_status"]
	return !hasStatus && inp.providerCount == 0
}

// Evaluate builds the full readiness report, checks sorted danger → ok.
func (r *ReadinessChecker) Evaluate(ctx context.Context) (*ReadinessReport, error) {
	inp, err := r.gather(ctx)
	if err != nil {
		return nil, err
	}
	checks := []ReadinessCheck{
		r.checkEncryptionKey(inp),
		r.checkSetupStatus(inp),
		r.checkProviders(inp),
		r.checkAPIKeys(inp),
		r.checkTimezone(inp),
		r.checkBaseURL(inp),
		r.checkSMTP(inp),
		r.checkCORS(inp),
		r.checkTrustedProxies(inp),
		r.checkContentLog(inp),
		checkBootGuards(),
	}
	sort.SliceStable(checks, func(i, j int) bool {
		return severityOrder[checks[i].Severity] < severityOrder[checks[j].Severity]
	})

	report := &ReadinessReport{
		GeneratedAt: time.Now().UTC(),
		Tier:        license.G().CurrentTier(),
		SetupNeeded: func() bool {
			_, hasStatus := inp.settings["setup_wizard_status"]
			return !hasStatus && inp.providerCount == 0
		}(),
		Checks: checks,
	}
	for _, c := range checks {
		switch c.Severity {
		case SeverityDanger:
			report.Summary.Danger++
		case SeverityWarn:
			report.Summary.Warn++
		case SeverityInfo:
			report.Summary.Info++
		default:
			report.Summary.OK++
		}
	}
	return report, nil
}

func (r *ReadinessChecker) checkEncryptionKey(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "encryption_key", TitleKey: "doctor.check.encryptionKey.title"}
	if r.encryptionActive(inp) {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.encryptionKey.active"
		return c
	}
	if inp.plaintextSecrets > 0 {
		c.Severity = SeverityDanger
		c.DetailKey = "doctor.check.encryptionKey.plaintextProviders"
		c.DetailParams = map[string]any{"provider_count": inp.plaintextSecrets}
	} else {
		c.Severity = SeverityWarn
		c.DetailKey = "doctor.check.encryptionKey.notConfigured"
	}
	c.FixHintKey = "doctor.check.encryptionKey.hint"
	c.FixAction = FixOpenWizard
	return c
}

func (r *ReadinessChecker) checkSetupStatus(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "setup_wizard_status", TitleKey: "doctor.check.setupWizard.title"}
	switch inp.settings["setup_wizard_status"] {
	case "done":
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.setupWizard.done"
	case "skipped":
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.setupWizard.skipped"
		c.FixHintKey = "doctor.check.setupWizard.hint"
		c.FixAction = FixOpenWizard
	default: // absent or pending
		c.Severity = SeverityWarn
		c.DetailKey = "doctor.check.setupWizard.notRun"
		c.FixHintKey = "doctor.check.setupWizard.hint"
		c.FixAction = FixOpenWizard
	}
	return c
}

func (r *ReadinessChecker) checkProviders(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{
		ID:        "providers_configured",
		TitleKey:  "doctor.check.providers.title",
		DetailParams: map[string]any{"provider_count": inp.providerCount},
	}
	if inp.providerCount == 0 {
		c.Severity = SeverityWarn
		c.DetailKey = "doctor.check.providers.none"
		c.FixHintKey = "doctor.check.providers.hint"
		c.FixAction = FixOpenWizard
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.providers.ok"
	}
	return c
}

func (r *ReadinessChecker) checkAPIKeys(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{
		ID:        "api_key_exists",
		TitleKey:  "doctor.check.apiKey.title",
		DetailParams: map[string]any{"key_count": inp.apiKeyCount},
	}
	if inp.apiKeyCount == 0 {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.apiKey.none"
		c.FixHintKey = "doctor.check.apiKey.hint"
		c.FixAction = FixOpenWizard
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.apiKey.ok"
	}
	return c
}

func (r *ReadinessChecker) checkTimezone(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "timezone_configured", TitleKey: "doctor.check.timezone.title"}
	dbTZ := inp.settings["stats_timezone"]
	yamlTZ := r.cfg.Database.Timezone
	switch {
	case dbTZ != "" && yamlTZ != "" && dbTZ != yamlTZ:
		c.Severity = SeverityWarn
		c.DetailKey = "doctor.check.timezone.mismatch"
		c.DetailParams = map[string]any{"db_timezone": dbTZ, "config_timezone": yamlTZ}
		c.FixHintKey = "doctor.check.timezone.hint"
		c.FixAction = FixOpenSettings
	case dbTZ == "" && yamlTZ == "":
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.timezone.unset"
		c.FixHintKey = "doctor.check.timezone.hint"
		c.FixAction = FixOpenSettings
	default:
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.timezone.ok"
		if dbTZ != "" {
			c.DetailParams = map[string]any{"timezone": dbTZ}
		} else {
			c.DetailParams = map[string]any{"timezone": yamlTZ}
		}
	}
	return c
}

func (r *ReadinessChecker) checkBaseURL(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "gateway_base_url", TitleKey: "doctor.check.baseUrl.title"}
	if r.cfg.Gateway.BaseURL == "" && inp.settings["gateway_base_url"] == "" {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.baseUrl.unset"
		c.FixHintKey = "doctor.check.baseUrl.hint"
		c.FixAction = FixOpenSettings
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.baseUrl.ok"
	}
	return c
}

func (r *ReadinessChecker) checkSMTP(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "smtp_configured", TitleKey: "doctor.check.smtp.title"}
	if r.effSMTPHost() != "" {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.smtp.ok"
		return c
	}
	// SMTP is consumed only by the commercial build (key delivery email,
	// guardrail/DataLens notifications).
	if license.G().CurrentTier() == license.TierCommunity {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.smtp.communityUnset"
	} else {
		c.Severity = SeverityWarn
		c.DetailKey = "doctor.check.smtp.proUnset"
	}
	// Editable in Settings (P2) — point there instead of docs.
	c.FixHintKey = "doctor.check.smtp.hint"
	c.FixAction = FixOpenSettings
	return c
}

func (r *ReadinessChecker) checkCORS(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "cors_allowed_origins", TitleKey: "doctor.check.cors.title"}
	if len(r.effCORSOrigins()) == 0 {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.cors.unset"
		c.FixHintKey = "doctor.check.cors.hint"
		c.FixAction = FixOpenSettings
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.cors.ok"
	}
	return c
}

func (r *ReadinessChecker) checkTrustedProxies(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "trusted_proxies", TitleKey: "doctor.check.proxies.title"}
	if len(r.cfg.Server.TrustedProxies) == 0 {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.proxies.unset"
		c.FixHintKey = "doctor.check.proxies.hint"
		c.FixAction = FixDocs
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.proxies.ok"
	}
	return c
}

func (r *ReadinessChecker) checkContentLog(inp *readinessInputs) ReadinessCheck {
	c := ReadinessCheck{ID: "content_log_enabled", TitleKey: "doctor.check.contentLog.title"}
	if inp.settings["log_content"] == "true" {
		c.Severity = SeverityInfo
		c.DetailKey = "doctor.check.contentLog.enabled"
		c.FixHintKey = "doctor.check.contentLog.hint"
		c.FixAction = FixOpenSettings
	} else {
		c.Severity = SeverityOK
		c.DetailKey = "doctor.check.contentLog.disabled"
	}
	return c
}

// checkBootGuards is an always-ok informational entry: the insecure-default
// jwt_secret / admin.password checks are enforced fatally at boot
// (internal/app/phases.go buildAuth), so they cannot occur in a running
// system. The entry exists so the report visibly covers them.
func checkBootGuards() ReadinessCheck {
	return ReadinessCheck{
		ID:        "boot_security_guards",
		Severity:  SeverityOK,
		TitleKey:  "doctor.check.bootGuards.title",
		DetailKey: "doctor.check.bootGuards.ok",
	}
}

// ReadinessHandler serves the readiness report.
type ReadinessHandler struct {
	checker *ReadinessChecker
}

func NewReadinessHandler(checker *ReadinessChecker) *ReadinessHandler {
	return &ReadinessHandler{checker: checker}
}

// Report handles GET /admin/api/system/readiness (login-only, no
// RequireAction — same class as /system/info so every viewer sees it).
func (h *ReadinessHandler) Report(c *gin.Context) {
	report, err := h.checker.Evaluate(c.Request.Context())
	if err != nil {
		internalErr(c, err, "evaluate readiness failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}

// LogReadinessSummary evaluates the report at startup and logs the outcome —
// danger ids at Error, warn ids at Warn, counts at Info. Read-only and never
// fatal (boot already hard-exits on insecure secrets), so CLI/docker users
// get the same signal as dashboard users.
func LogReadinessSummary(db *gorm.DB, cfg *config.Config) {
	report, err := NewReadinessChecker(db, cfg).Evaluate(context.Background())
	if err != nil {
		slog.Warn("readiness check skipped", "error", err)
		return
	}
	var dangerIDs, warnIDs []string
	for _, c := range report.Checks {
		switch c.Severity {
		case SeverityDanger:
			dangerIDs = append(dangerIDs, c.ID)
		case SeverityWarn:
			warnIDs = append(warnIDs, c.ID)
		}
	}
	if len(dangerIDs) > 0 {
		slog.Error("readiness check failed", "check_ids", dangerIDs, "hint", "see admin dashboard Settings > config doctor")
	}
	if len(warnIDs) > 0 {
		slog.Warn("readiness warnings", "check_ids", warnIDs)
	}
	slog.Info("readiness summary", "danger", report.Summary.Danger, "warn", report.Summary.Warn, "info", report.Summary.Info, "ok", report.Summary.OK, "setup_needed", report.SetupNeeded)
}
