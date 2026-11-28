package install

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/crosslink/internal/config"
)

// Dispatch is the shared front-end for the crosslink binary's one-shot
// install-time modes. Both the community and the commercial main call it, so
// the behaviour cannot drift between the two builds:
//
//	-preflight [-preflight-json]   check the install-time configuration, exit
//	init [flags]                   interactive installer, exit
//
// It returns false when args do not select one of these modes, and the caller
// proceeds to start the server. When it returns true the process is about to
// exit; it never returns after starting anything.
func Dispatch(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-preflight", "--preflight":
		asJSON := false
		for _, a := range args[1:] {
			if a == "-preflight-json" || a == "--preflight-json" {
				asJSON = true
			}
		}
		PreflightCommand(os.Stdout, os.Stderr, asJSON)
		return true
	case "init":
		InitCommand(args[1:])
		return true
	}
	return false
}

// PreflightCommand loads the configuration, runs the install-time checks,
// writes the report and exits the process (0 pass, 1 fail). It loads WITHOUT
// the final Validate step so an invalid install-time field (a GM key of the
// wrong length, say) is reported on its own checklist line alongside every
// other problem — Validate would stop at the first. A config that cannot be
// PARSED at all is reported as a single failing check.
func PreflightCommand(stdout, stderr io.Writer, asJSON bool) {
	cfg, err := config.LoadUnvalidated("")
	if err != nil {
		cfg = nil
	}
	report := config.Preflight(cfg)
	if cfg == nil && err != nil && len(report.Checks) == 1 {
		report.Checks[0].Message = "configuration could not be loaded"
		report.Checks[0].Fix = err.Error()
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		fmt.Fprint(stderr, config.PreflightFormatText(report))
	}
	os.Exit(config.PreflightExit(report))
}

// InitCommand is the terminal front-end of `crosslink init`:
//
//	crosslink init [-out PATH] [-shape docker|bare] [-overwrite] [-non-interactive]
//
// Interactive use needs a TTY: an installer that cannot ask fails loudly
// rather than invent a master key nobody wrote down. For scripted installs,
// --non-interactive takes the answers from INIT_* environment variables and
// generates everything else. It writes one manifest and exits — nothing is
// started and no connection is opened.
func InitCommand(args []string) {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	out := fs.String("out", ".env", "manifest path to write, or '-' to stream it to stdout (for redirecting out of a container)")
	shape := fs.String("shape", "", "deployment shape: docker | bare (asked interactively when empty)")
	overwrite := fs.Bool("overwrite", false, "replace an existing manifest (it may hold the master key)")
	nonInteractive := fs.Bool("non-interactive", false, "do not prompt: read answers from INIT_* env vars, generate the rest")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	// Connection passwords belong to infrastructure the operator already
	// owns, so they are the only secrets the installer cannot generate.
	env := map[Field]string{
		FieldDeployment:    *shape,
		FieldDBPassword:    os.Getenv("INIT_DB_PASSWORD"),
		FieldRedisPassword: os.Getenv("INIT_REDIS_PASSWORD"),
		FieldAdminPassword: os.Getenv("INIT_ADMIN_PASSWORD"),
		FieldCurrency:      os.Getenv("INIT_BILLING_CURRENCY"),
		FieldCryptoMode:    os.Getenv("INIT_CRYPTO_MODE"),
	}

	var ask Ask
	if *nonInteractive {
		ask = AnswersFromEnv(env)
	} else {
		if !stdinIsTerminal(os.Stdin) {
			fmt.Fprintln(os.Stderr, "init: needs an interactive terminal (use a TTY, or pass --non-interactive with INIT_* env vars)")
			os.Exit(2)
		}
		ask = terminalAsker{env: env, in: bufio.NewReader(os.Stdin)}.ask
	}

	dep := Deployment(strings.ToLower(strings.TrimSpace(*shape)))
	if _, err := Run(ask, os.Stdout, *out, *overwrite, dep); err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// stdinIsTerminal reports whether f is a character device we can prompt on.
// Standard library only — the installer adds no dependency.
func stdinIsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// terminalAsker prompts on stdin. An INIT_* value present in the environment
// becomes that prompt's default (Enter accepts it). One shared reader serves
// every prompt: a per-prompt bufio.Reader would swallow buffered lines.
// terminalAsker prompts on stderr, never stdout: with `init -out -` stdout IS the
// manifest, so a prompt printed there would corrupt the redirected file.
type terminalAsker struct {
	env map[Field]string
	in  *bufio.Reader
}

func (t terminalAsker) ask(field Field, prompt, def string, secret bool) (string, error) {
	if v := strings.TrimSpace(t.env[field]); v != "" {
		def = v
	}
	switch {
	case secret && def != "":
		// A password that came from the environment is never echoed.
		fmt.Fprintf(os.Stderr, "%s [from environment, Enter to accept]: ", prompt)
	case secret:
		fmt.Fprintf(os.Stderr, "%s (input is visible): ", prompt)
	case def != "":
		fmt.Fprintf(os.Stderr, "%s [%s]: ", prompt, def)
	default:
		fmt.Fprintf(os.Stderr, "%s: ", prompt)
	}
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return def, nil
	}
	return line, nil
}
