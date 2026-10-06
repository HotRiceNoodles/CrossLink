// Package version exposes build metadata injected at compile time.
//
// All three variables are set via -ldflags, e.g.:
//
//	go build -ldflags "-X github.com/crosslink/internal/version.Version=v1.0.0" ./cmd/server
//
// CI (goreleaser) and the Makefile build target always inject them; the
// defaults below only appear in bare `go build`/`go run` invocations.
package version

var (
	// Version is the release version, derived from git tags (e.g. "v0.2.0").
	Version = "dev"

	// Commit is the short git hash the binary was built from.
	Commit = "none"

	// Date is the build date in RFC 3339 format (UTC).
	Date = "unknown"
)

// String returns a human-readable build identifier, e.g.
// "v0.2.0+abc1234 (2026-10-06)".
func String() string {
	return Version + "+" + Commit + " (" + Date + ")"
}
