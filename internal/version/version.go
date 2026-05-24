package version

// Version is set at build time via -ldflags.
// Example: -ldflags "-X github.com/crosslink/internal/version.Version=v1.0.0"
var Version = "dev"
