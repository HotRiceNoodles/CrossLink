//go:build !license_dev

package license

// devEscapeHatchAllowed reports whether verify_mode=off (the signature
// verification escape hatch) is compiled into this binary. PRODUCTION
// builds (the default, no build tag) compile this to false: an operator
// setting CL_LICENSE_VERIFY_MODE=off / license.verify_mode=off is IGNORED
// with a loud note and verification falls back to the fail-closed auto
// semantics — forged tokens are rejected, valid signed licenses keep
// working. The config-driven attack chain (dev compose copied to prod +
// self-issued unsigned token) is dead in this build.
//
// Development builds (`go build -tags license_dev`, e.g. via the
// commercial `make build-dev`) compile devescape_dev.go instead and keep
// honoring off explicitly.
//
// This is a var (not const) purely so in-package tests can simulate the
// dev posture via enableDevEscapeForTest; no config/env path can mutate it.
var devEscapeHatchAllowed = false
