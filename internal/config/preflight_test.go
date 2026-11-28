package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The contract that matters: preflight must flag EXACTLY what the boot guards
// in app.buildAuth refuse to start on. A divergence in either direction is a
// bug — "preflight passed but the server crash-loops" or "preflight blocks a
// config the server would accept".

func findCheck(r *PreflightReport, param string) *PreflightCheck {
	for i := range r.Checks {
		if r.Checks[i].Param == param {
			return &r.Checks[i]
		}
	}
	return nil
}

func goodConfig() *Config {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	return &Config{
		Billing: BillingConfig{Currency: "CNY"},
		Crypto:  CryptoConfig{Mode: "standard"},
		SecretManager: SecretManagerConfig{
			EncryptionKey: key, AllowWizardGeneration: false,
		},
		Admin: AdminConfig{
			JWTSecret: strings.Repeat("s", 48), Password: "a-strong-install-password",
		},
		Database: DatabaseConfig{Driver: "postgres", Host: "db", Port: 5432, DBName: "crosslink"},
	}
}

func TestPreflight_GoodInstallPasses(t *testing.T) {
	r := Preflight(goodConfig())
	if !r.Pass {
		t.Fatalf("expected pass, got:\n%s", PreflightFormatText(r))
	}
	for _, p := range []string{"CL_BILLING_CURRENCY", "CL_CRYPTO_MODE", "CL_ENCRYPTION_KEY", "CL_ADMIN_JWT_SECRET", "CL_ADMIN_PASSWORD", "CL_DATABASE_HOST"} {
		c := findCheck(r, p)
		if c == nil {
			t.Fatalf("missing check %s", p)
		}
		if c.Severity == PreflightError || c.Severity == PreflightWarn {
			t.Fatalf("%s = %s (%s), want ok", p, c.Severity, c.Message)
		}
	}
	if PreflightExit(r) != 0 {
		t.Fatal("exit code must be 0 on pass")
	}
}

// Boot-guard parity: every condition that makes the server os.Exit(1) must be
// an error here, and a config the guards accept must pass.
func TestPreflight_MatchesBootGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		param  string
	}{
		{"default jwt secret", func(c *Config) { c.Admin.JWTSecret = "change-me-to-a-random-secret" }, "CL_ADMIN_JWT_SECRET"},
		{"empty jwt secret", func(c *Config) { c.Admin.JWTSecret = "" }, "CL_ADMIN_JWT_SECRET"},
		{"short jwt secret", func(c *Config) { c.Admin.JWTSecret = strings.Repeat("x", 31) }, "CL_ADMIN_JWT_SECRET"},
		{"default admin password", func(c *Config) { c.Admin.Password = "changeme" }, "CL_ADMIN_PASSWORD"},
		{"missing encryption key", func(c *Config) { c.SecretManager.EncryptionKey = "" }, "CL_ENCRYPTION_KEY"},
		{"encryption key not base64", func(c *Config) { c.SecretManager.EncryptionKey = "not-base64!!" }, "CL_ENCRYPTION_KEY"},
		{"aes key wrong length", func(c *Config) {
			c.SecretManager.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 16))
		}, "CL_ENCRYPTION_KEY"},
		{"gm key wrong length", func(c *Config) {
			c.Crypto.Mode = "gm"
			c.SecretManager.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 32))
		}, "CL_ENCRYPTION_KEY"},
		{"malformed billing currency", func(c *Config) { c.Billing.Currency = "EURO" }, "CL_BILLING_CURRENCY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := goodConfig()
			tc.mutate(cfg)
			r := Preflight(cfg)
			c := findCheck(r, tc.param)
			if c == nil {
				t.Fatalf("no check for %s", tc.param)
			}
			if c.Severity != PreflightError {
				t.Fatalf("%s severity = %s, want error (the boot guard refuses this config)", tc.param, c.Severity)
			}
			if r.Pass || PreflightExit(r) != 1 {
				t.Fatal("report must fail with exit code 1")
			}
		})
	}
}

// gm mode with a 16-byte key is the VALID pairing — must pass (guards accept).
func TestPreflight_GMKeyLengthAccepted(t *testing.T) {
	cfg := goodConfig()
	cfg.Crypto.Mode = "gm"
	cfg.SecretManager.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 16))
	r := Preflight(cfg)
	if c := findCheck(r, "CL_ENCRYPTION_KEY"); c.Severity != PreflightError {
		// passes
	} else {
		t.Fatalf("gm 16-byte key must be accepted: %s", c.Message)
	}
	if !r.Pass {
		t.Fatalf("expected pass:\n%s", PreflightFormatText(r))
	}
}

// Currency undeclared is NOT an error — the wizard collects it on first run.
func TestPreflight_UndeclaredCurrencyIsInfoNotError(t *testing.T) {
	cfg := goodConfig()
	cfg.Billing.Currency = ""
	r := Preflight(cfg)
	c := findCheck(r, "CL_BILLING_CURRENCY")
	if c == nil || c.Severity != PreflightInfo {
		t.Fatalf("undeclared currency = %+v, want info", c)
	}
	if !r.Pass {
		t.Fatal("undeclared currency must not block start")
	}
}

// Wizard key generation on in a non-empty-key config: starts, but flagged.
func TestPreflight_WizardGenerationWarns(t *testing.T) {
	cfg := goodConfig()
	cfg.SecretManager.AllowWizardGeneration = true
	r := Preflight(cfg)
	if c := findCheck(r, "CL_ENCRYPTION_KEY"); c.Severity != PreflightWarn {
		t.Fatalf("wizard generation = %s, want warn", c.Severity)
	}
	if !r.Pass {
		t.Fatal("a warning must not fail the report")
	}
}

// Every failing item is reported at once, and errors sort above warnings.
func TestPreflight_ReportsAllErrorsSorted(t *testing.T) {
	cfg := goodConfig()
	cfg.Admin.JWTSecret = ""             // error
	cfg.Admin.Password = "changeme"      // error
	cfg.SecretManager.EncryptionKey = "" // error
	cfg.Billing.Currency = ""            // info
	r := Preflight(cfg)
	if r.Pass {
		t.Fatal("expected fail")
	}
	errors := 0
	for _, c := range r.Checks {
		if c.Severity == PreflightError {
			errors++
		}
	}
	if errors != 3 {
		t.Fatalf("reported %d errors, want 3 (all at once, not one-per-crash)", errors)
	}
	// First non-error must come after every error.
	seenNonError := false
	for _, c := range r.Checks {
		if c.Severity != PreflightError {
			seenNonError = true
		} else if seenNonError {
			t.Fatal("an error is sorted below a non-error line")
		}
	}
}

func TestPreflight_NilConfig(t *testing.T) {
	r := Preflight(nil)
	if r.Pass || len(r.Checks) != 1 || r.Checks[0].Severity != PreflightError {
		t.Fatal("nil config must yield a single error")
	}
}

func TestPreflight_ConnectivityVariants(t *testing.T) {
	// sqlite without a path is an error; with a path it passes.
	cfg := goodConfig()
	cfg.Database = DatabaseConfig{Driver: "sqlite"}
	if r := Preflight(cfg); r.Pass {
		t.Fatal("sqlite without path must fail")
	}
	cfg.Database.SQLitePath = "/data/x.db"
	if r := Preflight(cfg); !r.Pass {
		t.Fatalf("sqlite with path must pass:\n%s", PreflightFormatText(r))
	}
	// Unknown driver.
	cfg.Database = DatabaseConfig{Driver: "oracle"}
	if r := Preflight(cfg); r.Pass {
		t.Fatal("unknown driver must fail")
	}
}

// The env mapping for the two install-time decisions must actually reach the
// config through Load (viper only resolves env for keys it knows — the whole
// reason these needed registering), and a currency typo must fail at load
// time rather than be silently normalized to CNY downstream.
func TestLoad_InstallTimeEnvMapping(t *testing.T) {
	t.Run("currency env reaches config", func(t *testing.T) {
		t.Setenv("CL_BILLING_CURRENCY", "USD")
		t.Setenv("CL_CRYPTO_MODE", "gm")
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Billing.Currency != "USD" {
			t.Fatalf("CL_BILLING_CURRENCY not honored: got %q", cfg.Billing.Currency)
		}
		if cfg.Crypto.Mode != "gm" {
			t.Fatalf("CL_CRYPTO_MODE not honored: got %q", cfg.Crypto.Mode)
		}
	})

	t.Run("currency typo rejected at load", func(t *testing.T) {
		t.Setenv("CL_BILLING_CURRENCY", "EURO")
		if _, err := Load(""); err == nil {
			t.Fatal("CL_BILLING_CURRENCY=EURO must fail Load, not be normalized to some default")
		}
	})

	// The currency is the operator's choice: any well-formed ISO 4217-style
	// code loads, and a lower-case spelling is the same code.
	t.Run("any currency code accepted", func(t *testing.T) {
		for _, code := range []string{"EUR", "JPY", "GBP", "HKD", "TWD", "KRW", "CHF", "INR", "BRL", "AED"} {
			t.Setenv("CL_BILLING_CURRENCY", code)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("CL_BILLING_CURRENCY=%s must load: %v", code, err)
			}
			if cfg.Billing.Currency != code {
				t.Fatalf("currency = %q, want %q", cfg.Billing.Currency, code)
			}
		}
	})

	t.Run("lower-case currency normalized", func(t *testing.T) {
		t.Setenv("CL_BILLING_CURRENCY", " jpy ")
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Billing.Currency != "JPY" {
			t.Fatalf("currency = %q, want JPY (trimmed and upper-cased)", cfg.Billing.Currency)
		}
	})

	t.Run("currency undeclared by default", func(t *testing.T) {
		cfg, err := Load("")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if cfg.Billing.Currency != "" {
			t.Fatalf("default currency = %q, want empty (wizard collects it)", cfg.Billing.Currency)
		}
	})
}

// Any well-formed currency code passes preflight; only a malformed value is
// an error. Preflight normalizes itself because it runs on an UNvalidated
// config (Load skips validation for it so every problem can be listed).
func TestPreflight_AnyCurrencyCodeAccepted(t *testing.T) {
	for _, code := range []string{"CNY", "USD", "EUR", "JPY", "GBP", "HKD", "TWD", "KRW", "CHF", "INR", "BRL", "AED"} {
		cfg := goodConfig()
		cfg.Billing.Currency = code
		c := findCheck(Preflight(cfg), "CL_BILLING_CURRENCY")
		if c == nil || c.Severity != PreflightOK {
			t.Fatalf("currency %s = %+v, want ok", code, c)
		}
		if c.Message != code {
			t.Fatalf("currency %s reported as %q", code, c.Message)
		}
	}
	// A lower-case declaration is the same code.
	cfg := goodConfig()
	cfg.Billing.Currency = " eur "
	if c := findCheck(Preflight(cfg), "CL_BILLING_CURRENCY"); c.Severity != PreflightOK || c.Message != "EUR" {
		t.Fatalf("lower-case currency = %+v, want ok EUR", c)
	}
	for _, code := range []string{"EURO", "US", "US1", "US$"} {
		cfg := goodConfig()
		cfg.Billing.Currency = code
		if c := findCheck(Preflight(cfg), "CL_BILLING_CURRENCY"); c.Severity != PreflightError {
			t.Fatalf("malformed currency %q = %s, want error", code, c.Severity)
		}
	}
}
