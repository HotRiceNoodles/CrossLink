package install

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func dockerAnswers() Answers {
	return Answers{
		Deployment:    DeploymentDocker,
		DBPassword:    "db-pass-123",
		RedisPassword: "redis-pass-123",
		Currency:      "CNY",
		CryptoMode:    "standard",
	}
}

// The contract the installer exists for: a manifest it generates must be
// accepted by preflight. We cannot import config here without a cycle in
// intent (install is a leaf), so pin the SHAPE preflight needs instead:
// every required variable present, secrets strong enough, key length right.
func TestBuild_DockerManifestShape(t *testing.T) {
	r, err := Build(dockerAnswers())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, k := range []string{"DB_PASSWORD", "REDIS_PASSWORD", "JWT_SECRET", "ADMIN_PASSWORD", "ENCRYPTION_KEY", "GATEWAY_AUTH_KEY", "BILLING_CURRENCY", "CRYPTO_MODE"} {
		if !strings.Contains(r.Manifest, "\n"+k+"=") {
			t.Fatalf("manifest lacks %s:\n%s", k, r.Manifest)
		}
	}
	// Short names only — docker compose maps them to CL_* itself.
	if strings.Contains(r.Manifest, "CL_") {
		t.Fatalf("docker manifest must use compose short names, found CL_*:\n%s", r.Manifest)
	}
	// The secrets preflight insists on: JWT >= 32 chars, key decodes to 32B.
	if len(r.Generated["JWT_SECRET"]) < 32 {
		t.Fatalf("jwt secret too short: %d", len(r.Generated["JWT_SECRET"]))
	}
	if got := len(r.Generated["ADMIN_PASSWORD"]); got != 20 {
		t.Fatalf("admin password length = %d, want 20", got)
	}
	for _, c := range r.Generated["ADMIN_PASSWORD"] {
		if !strings.ContainsRune(alnum, c) {
			t.Fatalf("admin password has non-typeable char %q", c)
		}
	}
}

// The bare shape must speak the process's own language: systemd feeds the
// EnvironmentFile verbatim, so CL_* names and the connection settings that
// compose hard-codes are both required.
func TestBuild_BareManifestUsesCLNames(t *testing.T) {
	a := dockerAnswers()
	a.Deployment = DeploymentBare
	a.Currency = "USD"
	r, err := Build(a)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, k := range []string{"CL_DATABASE_PASSWORD", "CL_REDIS_PASSWORD", "CL_ADMIN_JWT_SECRET", "CL_ADMIN_PASSWORD", "CL_ENCRYPTION_KEY", "CL_GATEWAY_AUTH_KEY", "CL_BILLING_CURRENCY", "CL_CRYPTO_MODE", "CL_DATABASE_HOST", "CL_REDIS_HOST"} {
		if !strings.Contains(r.Manifest, "\n"+k+"=") {
			t.Fatalf("bare manifest lacks %s:\n%s", k, r.Manifest)
		}
	}
	for _, short := range []string{"JWT_SECRET=", "ENCRYPTION_KEY=", "DB_PASSWORD="} {
		if strings.Contains(r.Manifest, "\n"+short) {
			t.Fatalf("bare manifest must not use compose short name %q", short)
		}
	}
	if !strings.Contains(r.Manifest, `CL_BILLING_CURRENCY="USD"`) {
		t.Fatalf("currency not written:\n%s", r.Manifest)
	}
}

func TestBuild_GMKeyIs16Bytes(t *testing.T) {
	a := dockerAnswers()
	a.CryptoMode = "gm"
	r, err := Build(a)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 16 raw bytes → 24 base64 chars (no padding boundary issue at 16: "xx==" is 24).
	if got := len(r.Generated["ENCRYPTION_KEY"]); got != 24 {
		t.Fatalf("gm key b64 length = %d, want 24 (16 bytes)", got)
	}
	if !strings.Contains(r.Manifest, `CRYPTO_MODE="gm"`) {
		t.Fatal("gm mode not written")
	}
}

func TestBuild_RejectsBadAnswers(t *testing.T) {
	cases := map[string]func(*Answers){
		"bad deployment":  func(a *Answers) { a.Deployment = "k8s" },
		"no db password":  func(a *Answers) { a.DBPassword = " " },
		"no redis passwd": func(a *Answers) { a.RedisPassword = "" },
		"bad currency":    func(a *Answers) { a.Currency = "EURO" },
		"bad crypto mode": func(a *Answers) { a.CryptoMode = "aes" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := dockerAnswers()
			mutate(&a)
			if _, err := Build(a); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// The settlement currency is the operator's choice: any well-formed code is
// written to the manifest as-is, in canonical (upper-case) form.
func TestBuild_AnyCurrencyCode(t *testing.T) {
	for _, code := range []string{"CNY", "USD", "EUR", "JPY", "GBP", "HKD", "TWD", "KRW", "CHF", "INR", "BRL", "AED"} {
		a := dockerAnswers()
		a.Currency = code
		r, err := Build(a)
		if err != nil {
			t.Fatalf("currency %s must be accepted: %v", code, err)
		}
		if !strings.Contains(r.Manifest, `BILLING_CURRENCY="`+code+`"`) {
			t.Fatalf("currency %s not written:\n%s", code, r.Manifest)
		}
	}
	// A lower-case answer is the same code.
	a := dockerAnswers()
	a.Currency = "eur"
	r, err := Build(a)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(r.Manifest, `BILLING_CURRENCY="EUR"`) {
		t.Fatal("lower-case currency must be normalized to EUR")
	}
}

func TestBuild_NormalizesCaseAndSpace(t *testing.T) {
	a := dockerAnswers()
	a.Currency = " usd "
	a.CryptoMode = " GM "
	r, err := Build(a)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(r.Manifest, `BILLING_CURRENCY="USD"`) || !strings.Contains(r.Manifest, `CRYPTO_MODE="gm"`) {
		t.Fatal("currency/mode not normalized")
	}
}

// Values are quoted so passwords with spaces or # survive dotenv parsing.
func TestBuild_QuotesValues(t *testing.T) {
	a := dockerAnswers()
	a.DBPassword = `p@ss w#rd \`
	r, err := Build(a)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(r.Manifest, `DB_PASSWORD="p@ss w#rd \\"`) {
		t.Fatalf("password not quoted/escaped:\n%s", r.Manifest)
	}
}

func TestWrite_RefusesToClobberMasterKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("ENCRYPTION_KEY=precious\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Build(dockerAnswers())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, r, false); err == nil {
		t.Fatal("must refuse to overwrite without --overwrite")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "ENCRYPTION_KEY=precious\n" {
		t.Fatal("existing manifest was modified")
	}
	if err := Write(path, r, true); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != r.Manifest {
		t.Fatal("overwrite did not replace content")
	}
}

func TestWrite_CreatesDirAndSets0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", ".env")
	r, err := Build(dockerAnswers())
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(path, r, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows only models the read-only bit, so the 0600 restriction can
	// only be asserted where POSIX permissions exist (the production target).
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("manifest permissions = %o, want 600 (it holds the master key)", perm)
		}
	}
	if r.Path != path {
		t.Fatalf("Result.Path = %q, want %q", r.Path, path)
	}
}

// The interview drives Build from scripted answers, and an error from any
// prompt aborts without writing anything.
func TestRun_EndToEndAndAbortOnPromptError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	var fields []Field
	answers := []string{"bare", "dbpw", "redispw", "", "USD", "gm"}
	ask := func(field Field, prompt, def string, secret bool) (string, error) {
		fields = append(fields, field)
		out := answers[0]
		answers = answers[1:]
		return out, nil
	}
	var buf bytes.Buffer
	r, err := Run(ask, &buf, path, false, "")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(answers) != 0 {
		t.Fatalf("not all answers consumed: %v", answers)
	}
	// The deployment prompt comes first and every field is asked exactly once.
	want := []Field{FieldDeployment, FieldDBPassword, FieldRedisPassword, FieldAdminPassword, FieldCurrency, FieldCryptoMode}
	if len(fields) != len(want) {
		t.Fatalf("asked %d fields, want %d", len(fields), len(want))
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("field[%d] = %s, want %s", i, fields[i], want[i])
		}
	}
	if r == nil || r.Path != path {
		t.Fatal("result/path not populated")
	}
	out := buf.String()
	if !strings.Contains(out, "systemctl start crosslink") {
		t.Fatalf("bare summary should name systemd:\n%s", out)
	}
	if !strings.Contains(out, "ENCRYPTION_KEY") {
		t.Fatal("summary must list the generated master key")
	}

	// A failing prompt aborts before any file is written.
	path2 := filepath.Join(dir, "second.env")
	failing := func(Field, string, string, bool) (string, error) {
		return "", os.ErrPermission
	}
	if _, err := Run(failing, &bytes.Buffer{}, path2, false, ""); err == nil {
		t.Fatal("expected the prompt error to propagate")
	}
	if _, err := os.Stat(path2); !os.IsNotExist(err) {
		t.Fatal("nothing must be written when the interview fails")
	}
}

func TestSummary_DockerNamesCompose(t *testing.T) {
	r, err := Build(dockerAnswers())
	if err != nil {
		t.Fatal(err)
	}
	if s := r.Summary(DeploymentDocker); !strings.Contains(s, "docker compose") {
		t.Fatalf("docker summary should name compose:\n%s", s)
	}
}

// A preset deployment skips the deployment prompt: scripted callers never
// see it, and the manifest still comes out in the right shape.
func TestRun_PresetDeploymentSkipsPrompt(t *testing.T) {
	var fields []Field
	answers := []string{"dbpw", "redispw", "", "CNY", "standard"}
	ask := func(field Field, _, _ string, _ bool) (string, error) {
		fields = append(fields, field)
		out := answers[0]
		answers = answers[1:]
		return out, nil
	}
	path := filepath.Join(t.TempDir(), "env")
	r, err := Run(ask, &bytes.Buffer{}, path, false, DeploymentBare)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fields[0] != FieldDBPassword {
		t.Fatalf("first prompted field = %s, want %s (deployment prompt must be skipped)", fields[0], FieldDBPassword)
	}
	if !strings.Contains(r.Manifest, "CL_ENCRYPTION_KEY=") {
		t.Fatal("preset bare shape must produce a CL_* manifest")
	}
}

// Regression: a non-interactive install with INIT_CRYPTO_MODE unset must
// default to standard (the interactive default), not fail on an empty mode.
func TestAnswersFromEnv_FallsBackToPromptDefault(t *testing.T) {
	ask := AnswersFromEnv(map[Field]string{
		FieldDBPassword:    "dbpw",
		FieldRedisPassword: "redis",
		FieldCurrency:      "USD",
		// FieldCryptoMode deliberately absent.
	})
	answers := Answers{}
	for _, f := range []struct {
		field  Field
		def    string
		secret bool
		target *string
	}{
		{FieldDBPassword, "", true, &answers.DBPassword},
		{FieldRedisPassword, "", true, &answers.RedisPassword},
		{FieldAdminPassword, "", true, &answers.AdminPassword},
		{FieldCurrency, "", false, &answers.Currency},
		{FieldCryptoMode, DefaultCryptoMode, false, &answers.CryptoMode},
	} {
		v, err := ask(f.field, "", f.def, f.secret)
		if err != nil {
			t.Fatalf("%s: %v", f.field, err)
		}
		*f.target = v
	}
	answers.Deployment = DeploymentDocker
	r, err := Build(answers)
	if err != nil {
		t.Fatalf("non-interactive install must succeed with the crypto mode defaulted: %v", err)
	}
	if !strings.Contains(r.Manifest, `CRYPTO_MODE="standard"`) {
		t.Fatal("crypto mode should default to standard")
	}
	// An admin password left unset is generated, never left empty.
	if r.Generated["ADMIN_PASSWORD"] == "" {
		t.Fatal("admin password must be generated when not supplied")
	}

	// Missing connection passwords are refused — the installer never
	// invents a password for infrastructure it does not own.
	answers.DBPassword = ""
	if _, err := Build(answers); err == nil || !strings.Contains(err.Error(), "database password") {
		t.Fatalf("missing db password must be rejected with a clear message, got %v", err)
	}
}

// The manifest is fed to systemd's EnvironmentFile and to dotenv parsers on
// hosts of any locale; keep its bytes pure ASCII so no parser can choke on an
// encoding (values are already ASCII by construction — this pins comments).
func TestBuild_ManifestIsASCII(t *testing.T) {
	for _, dep := range []Deployment{DeploymentDocker, DeploymentBare} {
		a := dockerAnswers()
		a.Deployment = dep
		r, err := Build(a)
		if err != nil {
			t.Fatalf("build %s: %v", dep, err)
		}
		for i, b := range []byte(r.Manifest) {
			if b > 0x7E || (b < 0x20 && b != '\n') {
				t.Fatalf("%s manifest byte %d = 0x%02x (non-ASCII): %q", dep, i, b, r.Manifest[max(0, i-20):min(len(r.Manifest), i+20)])
			}
		}
	}
}

// Dispatch is the shared entry for both builds: it must claim exactly the
// one-shot modes and nothing else. These cases must NOT exit, so they are
// safe to call in-process.
func TestDispatch_ClaimsOnlyOneShotModes(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{},
		{"-migrate-only"},
		{"-preflight-json"},
		{"--migrate-only"},
		{"serve"},
		{"initializer"}, // prefix of "init" must not match
		{"", "init"},    // init must be the FIRST argument
	} {
		if Dispatch(args) {
			t.Fatalf("Dispatch(%v) claimed the mode; it should let the server start", args)
		}
	}
}

// `init -out -` is how the installer is used from a container with no
// volume: stdout is redirected into the manifest file, so stdout must carry
// the manifest and NOTHING else — the summary goes to the same writer in the
// normal path and would corrupt the file.
func TestRun_OutDashStreamsManifestOnly(t *testing.T) {
	answers := []string{"dbpw", "redis", "", "CNY", "standard"}
	ask := func(Field, string, string, bool) (string, error) {
		out := answers[0]
		answers = answers[1:]
		return out, nil
	}
	var stdout bytes.Buffer
	r, err := Run(ask, &stdout, "-", false, DeploymentDocker)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stdout.String() != r.Manifest {
		t.Fatalf("stdout must be exactly the manifest.\n got extra:\n%q", strings.TrimPrefix(stdout.String(), r.Manifest))
	}
	// The summary must not have leaked into it.
	for _, marker := range []string{"Generated secrets", "Next steps", "Manifest written"} {
		if strings.Contains(stdout.String(), marker) {
			t.Fatalf("summary text %q leaked into the redirected manifest", marker)
		}
	}
	// Nothing was written to the working directory.
	if _, err := os.Stat("-"); err == nil {
		t.Fatal("a file literally named '-' must not be created")
	}
}
