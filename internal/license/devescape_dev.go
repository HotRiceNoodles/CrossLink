//go:build license_dev

package license

// devEscapeHatchAllowed enables verify_mode=off — see devescape_prod.go.
// Only reachable in builds tagged license_dev (make build-dev).
var devEscapeHatchAllowed = true
