package config

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// Preflight is the install-time configuration check: it runs the SAME rules
// the boot guards enforce (weak JWT secret, default admin password, malformed
// encryption key) BEFORE the server starts, and reports every problem at once
// as a checklist instead of the guards' one-error-then-exit crash loop. It
// never touches the database — safe to run from a container entrypoint, from
// `crosslink preflight`, or from the installer before first start.
//
// Severity:
//   error   — the server will refuse to start (or is unsafe to start)
//   warn    — starts, but the installation is incomplete or risky
//   ok/info — informational; the install manifest line is satisfied

// PreflightSeverity classifies one preflight finding.
type PreflightSeverity string

const (
	PreflightOK    PreflightSeverity = "ok"
	PreflightInfo  PreflightSeverity = "info"
	PreflightWarn  PreflightSeverity = "warn"
	PreflightError PreflightSeverity = "error"
)

// PreflightCheck is one line of the install-time report.
type PreflightCheck struct {
	// Param is the install-manifest parameter (env var name when one exists).
	Param     string            `json:"param"`
	Label     string            `json:"label"`
	Severity  PreflightSeverity `json:"severity"`
	Message   string            `json:"message"`
	Fix       string            `json:"fix,omitempty"`
	InstallOK bool              `json:"install_ok"` // true for ok/info lines
}

// PreflightReport is the full result.
type PreflightReport struct {
	Checks []PreflightCheck `json:"checks"`
	Pass   bool             `json:"pass"` // no error-severity findings
}

// HasErrors reports whether any check is at error severity.
func (r *PreflightReport) HasErrors() bool {
	for _, c := range r.Checks {
		if c.Severity == PreflightError {
			return true
		}
	}
	return false
}

// Preflight evaluates the install-time parameters of an already-loaded
// config. Deterministic order: every check runs; nothing short-circuits.
func Preflight(cfg *Config) *PreflightReport {
	if cfg == nil {
		return &PreflightReport{Checks: []PreflightCheck{{
			Param: "config", Label: "Configuration", Severity: PreflightError,
			Message: "config could not be loaded", Fix: "check config.yaml syntax / env var names (CL_ prefix)",
		}}}
	}
	var checks []PreflightCheck
	add := func(param, label string, sev PreflightSeverity, msg string, fix ...string) {
		f := ""
		if len(fix) > 0 {
			f = fix[0]
		}
		checks = append(checks, PreflightCheck{
			Param: param, Label: label, Severity: sev, Message: msg, Fix: f,
			InstallOK: sev == PreflightOK || sev == PreflightInfo,
		})
	}

	// ---- A-tier: the one-time parameters (install-time decisions) ----

	// Billing currency: "" is allowed (wizard will collect it); a non-empty
	// value must be a well-formed currency code. Checked here (not left to
	// Validate) so a typo is its own checklist line and every other problem
	// still shows.
	cur := NormalizeCurrencyCode(cfg.Billing.Currency)
	switch {
	case cur == "":
		add("CL_BILLING_CURRENCY", "Billing currency", PreflightInfo,
			"not declared — the setup wizard will ask on first run and lock it",
			"optional: set an ISO 4217 code (CNY, USD, EUR, JPY, ...) for headless installs")
	case ValidCurrencyCode(cur):
		add("CL_BILLING_CURRENCY", "Billing currency", PreflightOK, cur)
	default:
		add("CL_BILLING_CURRENCY", "Billing currency", PreflightError,
			fmt.Sprintf("%q is not a currency code", cfg.Billing.Currency),
			"use a 3-letter ISO 4217 code such as CNY, USD, EUR or JPY")
	}

	// Cryptography mode decides the key length required below; an unknown
	// mode is its own error, and the key check still runs with the standard
	// (32-byte) requirement so both problems are visible at once.
	gm := cfg.Crypto.Mode == "gm"
	modeLabel := "standard (AES)"
	if gm {
		modeLabel = "gm (SM4, 国密)"
	}
	switch cfg.Crypto.Mode {
	case "", "standard", "gm":
		add("CL_CRYPTO_MODE", "Cryptography mode", PreflightOK, modeLabel)
	default:
		add("CL_CRYPTO_MODE", "Cryptography mode", PreflightError,
			fmt.Sprintf("%q is not supported", cfg.Crypto.Mode), "use standard or gm")
	}

	// Master encryption key: the required length depends on the crypto mode.
	key := strings.TrimSpace(cfg.SecretManager.EncryptionKey)
	wantBytes := 32
	if gm {
		wantBytes = 16
	}
	switch {
	case key == "":
		add("CL_ENCRYPTION_KEY", "Master encryption key", PreflightError,
			"not set — provider API keys cannot be encrypted",
			fmt.Sprintf("generate: openssl rand -base64 %d  (store in your secret manager, back it up)", wantBytes))
	case cfg.SecretManager.AllowWizardGeneration:
		add("CL_ENCRYPTION_KEY", "Master encryption key", PreflightWarn,
			"allow_wizard_generation is enabled — a wizard-generated key lives in the same DB as the ciphertexts",
			"for production keep this false and provision the key via CL_ENCRYPTION_KEY")
	default:
		raw, err := base64.StdEncoding.DecodeString(key)
		switch {
		case err != nil:
			add("CL_ENCRYPTION_KEY", "Master encryption key", PreflightError,
				"not valid base64", fmt.Sprintf("generate: openssl rand -base64 %d", wantBytes))
		case len(raw) != wantBytes:
			add("CL_ENCRYPTION_KEY", "Master encryption key", PreflightError,
				fmt.Sprintf("decodes to %d bytes, %s mode needs %d", len(raw), cfg.Crypto.Mode, wantBytes),
				fmt.Sprintf("generate: openssl rand -base64 %d", wantBytes))
		default:
			add("CL_ENCRYPTION_KEY", "Master encryption key", PreflightOK,
				fmt.Sprintf("present (%d bytes, %s)", len(raw), modeLabel))
		}
	}

	// Admin credentials — mirrors the boot guards in app.buildAuth exactly.
	if cfg.Admin.IsDefaultJWTSecret() {
		add("CL_ADMIN_JWT_SECRET", "Admin JWT secret", PreflightError,
			"missing or a well-known default value",
			"generate: openssl rand -base64 48")
	} else if cfg.Admin.IsJWTSecretInsecure() {
		add("CL_ADMIN_JWT_SECRET", "Admin JWT secret", PreflightError,
			fmt.Sprintf("too short (%d chars, minimum 32)", len(cfg.Admin.JWTSecret)),
			"generate: openssl rand -base64 48")
	} else {
		add("CL_ADMIN_JWT_SECRET", "Admin JWT secret", PreflightOK, "set")
	}

	if cfg.Admin.IsDefaultPassword() {
		add("CL_ADMIN_PASSWORD", "Initial admin password", PreflightError,
			"missing or the default 'changeme'",
			"set a strong password (it is used once, to seed the admin user; changed in the UI afterwards)")
	} else if len(cfg.Admin.Password) < 12 {
		add("CL_ADMIN_PASSWORD", "Initial admin password", PreflightWarn,
			fmt.Sprintf("short (%d chars)", len(cfg.Admin.Password)),
			"12+ characters recommended; the first login forces a change anyway")
	} else {
		add("CL_ADMIN_PASSWORD", "Initial admin password", PreflightOK, "set")
	}

	// ---- Connection topology (needed to start, not one-time) ----
	checks = append(checks, preflightConnectivity(cfg)...)

	// Errors first, then warns, then ok — the reader sees what to fix on top.
	sort.SliceStable(checks, func(i, j int) bool {
		return preflightRank(checks[i].Severity) < preflightRank(checks[j].Severity)
	})
	return &PreflightReport{Checks: checks, Pass: !hasError(checks)}
}

func hasError(checks []PreflightCheck) bool {
	for _, c := range checks {
		if c.Severity == PreflightError {
			return true
		}
	}
	return false
}

func preflightRank(s PreflightSeverity) int {
	switch s {
	case PreflightError:
		return 0
	case PreflightWarn:
		return 1
	case PreflightInfo:
		return 2
	default:
		return 3
	}
}

// preflightConnectivity reports what the server will connect to. It does NOT
// dial: preflight must stay side-effect free and fast so it can gate a
// container entrypoint. Unknown drivers are errors (Validate rejects them).
func preflightConnectivity(cfg *Config) []PreflightCheck {
	ck := func(param, label string, sev PreflightSeverity, msg string) PreflightCheck {
		fix := ""
		if sev == PreflightError {
			fix = "set it in the install manifest (.env) or config.yaml"
		}
		return PreflightCheck{Param: param, Label: label, Severity: sev, Message: msg, Fix: fix,
			InstallOK: sev == PreflightOK || sev == PreflightInfo}
	}
	switch cfg.Database.Driver {
	case "", "postgres":
		if cfg.Database.Host == "" || cfg.Database.DBName == "" {
			return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightError, "postgres host/dbname not configured")}
		}
		return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightOK,
			fmt.Sprintf("postgres %s:%d/%s", cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName))}
	case "kingbase", "kingbasees":
		if cfg.Database.Host == "" || cfg.Database.DBName == "" {
			return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightError, "kingbase host/dbname not configured")}
		}
		return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightOK,
			fmt.Sprintf("kingbase %s:%d/%s", cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName))}
	case "mysql", "oceanbase":
		if cfg.Database.Host == "" || cfg.Database.DBName == "" {
			return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightError, "database host/dbname not configured")}
		}
		return []PreflightCheck{ck("CL_DATABASE_HOST", "Database", PreflightOK,
			fmt.Sprintf("%s %s:%d/%s", cfg.Database.Driver, cfg.Database.Host, cfg.Database.Port, cfg.Database.DBName))}
	case "sqlite":
		if cfg.Database.SQLitePath == "" {
			return []PreflightCheck{ck("CL_DATABASE_SQLITE_PATH", "Database", PreflightError, "sqlite_path not configured")}
		}
		return []PreflightCheck{ck("CL_DATABASE_SQLITE_PATH", "Database", PreflightOK, "sqlite "+cfg.Database.SQLitePath)}
	default:
		return []PreflightCheck{ck("CL_DATABASE_DRIVER", "Database", PreflightError,
			fmt.Sprintf("unsupported driver %q", cfg.Database.Driver))}
	}
}

// PreflightFormatText renders the report as the human-readable checklist.
func PreflightFormatText(r *PreflightReport) string {
	var b strings.Builder
	b.WriteString("CrossLink install preflight\n")
	for _, c := range r.Checks {
		mark := "  "
		switch c.Severity {
		case PreflightOK:
			mark = "OK  "
		case PreflightInfo:
			mark = "INFO"
		case PreflightWarn:
			mark = "WARN"
		case PreflightError:
			mark = "FAIL"
		}
		b.WriteString(fmt.Sprintf("  [%s] %-24s %s\n", mark, c.Label, c.Message))
		if c.Fix != "" {
			b.WriteString(fmt.Sprintf("         %-24s -> %s\n", "", c.Fix))
		}
	}
	if r.Pass {
		b.WriteString("Result: PASS — ready to start. Finish the remaining setup in the web wizard on first run.\n")
	} else {
		b.WriteString("Result: FAIL — fix the items marked FAIL above, then start again.\n")
	}
	return b.String()
}

// PreflightExit returns the process exit code for a report (0 pass, 1 fail).
func PreflightExit(r *PreflightReport) int {
	if r == nil || r.Pass {
		return 0
	}
	return 1
}
