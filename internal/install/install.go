// Package install is the interactive installer behind `crosslink init`.
//
// It interviews the operator for the install-time decisions (the A-tier
// parameters that are fixed for the deployment's lifetime), GENERATES every
// secret instead of asking for one, and writes a single install manifest that
// the chosen deployment shape consumes directly:
//
//	docker  → .env next to the compose file (compose maps short names to CL_*)
//	bare    → an EnvironmentFile for systemd (systemd feeds CL_* to the process)
//
// Design constraints:
//   - It never touches the database and never starts the server. DB-side
//     settings (timezone, the write-once currency row) stay with the setup
//     wizard — one convergence point, no second "half initialized" state.
//   - Answers arrive through an Ask callback so the flow is testable without
//     a terminal; the terminal front-end lives in cmd/server.
//   - The manifest is exactly what preflight validates, so
//     `crosslink init && crosslink -preflight` is green by construction.
package install

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crosslink/internal/config"
)

// Deployment is the operator's deployment shape. It decides which VARIABLE
// NAMES the manifest uses — compose translates short names to CL_* itself,
// whereas an EnvironmentFile is fed to the process verbatim.
type Deployment string

const (
	DeploymentDocker Deployment = "docker"
	DeploymentBare   Deployment = "bare"
)

// Field identifies what a prompt is asking for, so a front-end can supply an
// environment default (or a scripted answer) without parsing prompt text.
type Field string

const (
	FieldDeployment    Field = "deployment"
	FieldDBPassword    Field = "db_password"
	FieldRedisPassword Field = "redis_password"
	FieldAdminPassword Field = "admin_password"
	FieldCurrency      Field = "currency"
	FieldCryptoMode    Field = "crypto_mode"
)

// Ask obtains the answer for one field. secret=true marks a password prompt
// (the front-end may hide the echo). An empty def means "no default"; a
// caller returning "" selects the default.
type Ask func(field Field, prompt, def string, secret bool) (string, error)

// Answers is what the operator chose.
type Answers struct {
	Deployment    Deployment
	DBPassword    string
	RedisPassword string
	// AdminPassword may be "" → generated and reported once.
	AdminPassword string
	// Currency is an ISO 4217-style code (CNY, USD, EUR, JPY, ...), or "" to
	// choose in the setup wizard.
	Currency   string
	CryptoMode string // standard | gm
}

// DefaultCryptoMode matches the config default.
const DefaultCryptoMode = "standard"

// variable maps a logical setting to its manifest name per deployment shape.
func variable(dep Deployment, short, long string) string {
	if dep == DeploymentBare {
		return long
	}
	return short
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// encryptionKeyBytes is the master-key length for a crypto mode.
func encryptionKeyBytes(mode string) int {
	if mode == "gm" {
		return 16
	}
	return 32
}

// validate rejects answers preflight would reject, with clearer wording.
// Preflight still runs afterwards as the backstop.
func (a Answers) validate() error {
	if a.Deployment != DeploymentDocker && a.Deployment != DeploymentBare {
		return fmt.Errorf("deployment must be %q or %q", DeploymentDocker, DeploymentBare)
	}
	if strings.TrimSpace(a.DBPassword) == "" {
		return fmt.Errorf("database password is required")
	}
	if strings.TrimSpace(a.RedisPassword) == "" {
		return fmt.Errorf("redis password is required")
	}
	if cur := config.NormalizeCurrencyCode(a.Currency); cur != "" && !config.ValidCurrencyCode(cur) {
		return fmt.Errorf("currency must be a 3-letter ISO 4217 code such as CNY, USD or EUR (or empty to choose in the wizard), got %q", a.Currency)
	}
	switch strings.ToLower(strings.TrimSpace(a.CryptoMode)) {
	case "standard", "gm":
	default:
		return fmt.Errorf("crypto mode must be standard or gm, got %q", a.CryptoMode)
	}
	return nil
}

// quoteEnv renders a value for a dotenv / EnvironmentFile line. systemd's
// EnvironmentFile and compose both accept double-quoted values, so quoting
// unconditionally keeps passwords containing spaces or # intact.
func quoteEnv(v string) string {
	return `"` + strings.ReplaceAll(v, `\`, `\\`) + `"`
}

// Result is what the installer produced.
type Result struct {
	// Path is the manifest file that was written ("" for a dry run).
	Path string
	// Manifest is the file content.
	Manifest string
	// Generated holds the secrets the installer created, keyed by the
	// manifest variable name, so the operator can copy them into a password
	// manager.
	Generated map[string]string
}

// Build turns answers into the manifest, generating every secret. Pure: it
// writes nothing and reads nothing from the environment.
func Build(a Answers) (*Result, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	dep := a.Deployment
	gen := map[string]string{}
	mk := func(short, long string, n int) (string, error) {
		v, err := randomB64(n)
		if err != nil {
			return "", err
		}
		gen[variable(dep, short, long)] = v
		return v, nil
	}

	jwt, err := mk("JWT_SECRET", "CL_ADMIN_JWT_SECRET", 48)
	if err != nil {
		return nil, err
	}
	// The admin password must be typeable: 18 random bytes of base64 is
	// unwieldy and can contain characters some shells mangle. Use a
	// alphanumeric alphabet instead.
	admin := a.AdminPassword
	if admin == "" {
		if admin, err = randomAlnum(20); err != nil {
			return nil, err
		}
		gen[variable(dep, "ADMIN_PASSWORD", "CL_ADMIN_PASSWORD")] = admin
	}
	enc, err := mk("ENCRYPTION_KEY", "CL_ENCRYPTION_KEY", encryptionKeyBytes(a.CryptoMode))
	if err != nil {
		return nil, err
	}
	gateway, err := mk("GATEWAY_AUTH_KEY", "CL_GATEWAY_AUTH_KEY", 32)
	if err != nil {
		return nil, err
	}

	name := func(short, long string) string { return variable(dep, short, long) }
	currency := config.NormalizeCurrencyCode(a.Currency)
	mode := strings.ToLower(strings.TrimSpace(a.CryptoMode))

	var b strings.Builder
	fmt.Fprintf(&b, manifestHeader, dep)
	line := func(key, val, comment string) {
		if comment != "" {
			b.WriteString(comment)
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s=%s\n", key, quoteEnv(val))
	}
	line(name("DB_PASSWORD", "CL_DATABASE_PASSWORD"), a.DBPassword, "# --- required: connections ---")
	line(name("REDIS_PASSWORD", "CL_REDIS_PASSWORD"), a.RedisPassword, "")
	b.WriteString("\n# --- required: secrets (GENERATED by `crosslink init`; back this file up) ---\n")
	line(name("JWT_SECRET", "CL_ADMIN_JWT_SECRET"), jwt, "# Admin session signing key. Rotating it logs every admin out.")
	line(name("ADMIN_PASSWORD", "CL_ADMIN_PASSWORD"), admin, "# Used ONCE to seed the admin user; the first login forces a change.")
	line(name("ENCRYPTION_KEY", "CL_ENCRYPTION_KEY"), enc, "# Master key for provider secrets. UNRECOVERABLE if lost - back it up.")
	line(name("GATEWAY_AUTH_KEY", "CL_GATEWAY_AUTH_KEY"), gateway, "# Protects the /metrics endpoint.")
	b.WriteString("\n# --- install-time decisions (fixed for the deployment's lifetime) ---\n")
	line(name("BILLING_CURRENCY", "CL_BILLING_CURRENCY"), currency,
		"# Settlement currency: any 3-letter ISO 4217 code (CNY, USD, EUR, JPY, ...).\n# The ONE currency everything is priced in; locked once written.\n# Empty = choose in the setup wizard on first run.")
	line(name("CRYPTO_MODE", "CL_CRYPTO_MODE"), mode,
		"# standard (AES) | gm (SM2/SM3/SM4). gm needs a 16-byte ENCRYPTION_KEY.")

	if dep == DeploymentBare {
		// systemd feeds these straight to the process, so the connection
		// settings compose hard-codes must be declared here too.
		b.WriteString("\n# --- connection settings (edit to match your server) ---\n")
		line("CL_DATABASE_HOST", "localhost", "")
		line("CL_DATABASE_PORT", "5432", "")
		line("CL_DATABASE_DBNAME", "crosslink", "")
		line("CL_REDIS_HOST", "localhost", "")
		line("CL_REDIS_PORT", "6379", "")
	}
	return &Result{Manifest: b.String(), Generated: gen}, nil
}

const manifestHeader = `# CrossLink install manifest - generated by ` + "`crosslink init`" + ` for a %s deployment.
# This file holds secrets. Keep it at mode 600 and back it up in your secret
# manager: ENCRYPTION_KEY cannot be recovered if lost. Re-run
# ` + "`crosslink -preflight`" + ` after any edit.
`

const alnum = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// randomAlnum returns n characters from a typeable, unambiguous alphabet
// (no 0/O/1/l/I). Rejection sampling keeps the distribution uniform — a
// plain `byte % len(alnum)` is biased because 256 is not a multiple of 58.
func randomAlnum(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 1)
	// 232 is the largest multiple of 58 that fits in a byte; bytes at or
	// above it are discarded.
	const limit = 232
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, alnum[int(buf[0])%len(alnum)])
	}
	return string(out), nil
}

// Write persists the manifest. It refuses to clobber an existing file unless
// overwrite is set: an install manifest holds the master key, and silently
// replacing it would orphan every encrypted provider secret.
func Write(path string, r *Result, overwrite bool) error {
	if path == "" {
		return fmt.Errorf("manifest path is required")
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists — refusing to overwrite (it may hold the master key; use --overwrite to replace it)", path)
		}
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}
	// 0600: the manifest holds secrets. Temp file + rename so a crash
	// mid-write never leaves a half-written manifest behind.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".crosslink-init-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.WriteString(r.Manifest); err != nil {
		tmp.Close()
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("set manifest permissions: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("place manifest: %w", err)
	}
	r.Path = path
	return nil
}

// GeneratedNames lists the generated variable names in stable order.
func (r *Result) GeneratedNames() []string {
	out := make([]string, 0, len(r.Generated))
	for k := range r.Generated {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Summary renders the generated secrets and the next steps for the chosen
// deployment shape.
func (r *Result) Summary(dep Deployment) string {
	var b strings.Builder
	b.WriteString("\nGenerated secrets — save these now (ENCRYPTION_KEY is unrecoverable if lost):\n")
	for _, k := range r.GeneratedNames() {
		fmt.Fprintf(&b, "  %-22s %s\n", k, r.Generated[k])
	}
	fmt.Fprintf(&b, "\nManifest written to %s\n", r.Path)
	b.WriteString("\nNext steps:\n")
	if dep == DeploymentDocker {
		b.WriteString("  1. Keep the .env file safe; it holds the master key.\n")
		b.WriteString("  2. docker compose -f deployments/docker-compose.prod.yaml up -d\n")
	} else {
		b.WriteString("  1. Keep the env file safe; it holds the master key.\n")
		fmt.Fprintf(&b, "  2. Point EnvironmentFile= in the systemd unit at %s, then: systemctl start crosslink\n", r.Path)
	}
	b.WriteString("  3. Open the admin UI and finish the setup wizard:\n")
	b.WriteString("     statistics timezone, gateway URL, the billing currency (if left empty),\n")
	b.WriteString("     then connect your first provider and model.\n")
	return b.String()
}

// Run is the interactive flow: interview → build → write → summary.
//
// deployment may be preset ("" = ask). When it is preset the deployment
// prompt is skipped entirely, so scripted callers never see it.
func Run(ask Ask, out io.Writer, manifestPath string, overwrite bool, deployment Deployment) (*Result, error) {
	dep := deployment
	if dep == "" {
		depIn, err := ask(FieldDeployment, "Deployment shape [docker/bare]", string(DeploymentDocker), false)
		if err != nil {
			return nil, err
		}
		dep = Deployment(strings.ToLower(strings.TrimSpace(depIn)))
		if dep == "" {
			dep = DeploymentDocker
		}
	}
	dbpw, err := ask(FieldDBPassword, "Database password", "", true)
	if err != nil {
		return nil, err
	}
	redisPw, err := ask(FieldRedisPassword, "Redis password", "", true)
	if err != nil {
		return nil, err
	}
	admin, err := ask(FieldAdminPassword, "Initial admin password (empty = generate one)", "", true)
	if err != nil {
		return nil, err
	}
	cur, err := ask(FieldCurrency, "Billing currency: a 3-letter ISO 4217 code such as CNY, USD, EUR, JPY (empty = choose in the web wizard)", "", false)
	if err != nil {
		return nil, err
	}
	mode, err := ask(FieldCryptoMode, "Cryptography standard|gm", DefaultCryptoMode, false)
	if err != nil {
		return nil, err
	}
	res, err := Build(Answers{
		Deployment:    dep,
		DBPassword:    dbpw,
		RedisPassword: redisPw,
		AdminPassword: admin,
		Currency:      cur,
		CryptoMode:    mode,
	})
	if err != nil {
		return nil, err
	}
	// manifestPath "-" streams the manifest to `out` and nothing else: the
	// summary (which also goes to out) would corrupt a redirected file. This
	// is how the installer is used from a container that has no volume to
	// write to — `docker run ... init -out - > .env`.
	if manifestPath == "-" {
		fmt.Fprint(out, res.Manifest)
		return res, nil
	}
	if err := Write(manifestPath, res, overwrite); err != nil {
		return nil, err
	}
	fmt.Fprint(out, res.Summary(dep))
	return res, nil
}

// AnswersFromEnv builds a non-interactive Ask from pre-supplied values: a
// field is answered from the map when present, otherwise from the prompt's
// own default — so an omitted crypto mode still defaults to standard, exactly
// as it would when answered interactively. The two connection passwords have
// no default; Build rejects them empty with a clear message rather than the
// installer inventing one.
func AnswersFromEnv(values map[Field]string) Ask {
	return func(field Field, _, def string, _ bool) (string, error) {
		if v := strings.TrimSpace(values[field]); v != "" {
			return v, nil
		}
		return def, nil
	}
}
