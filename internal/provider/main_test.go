package provider

import (
	"os"
	"testing"
)

// TestMain disables the production SSRF dialer for this package's tests so that
// providers built via the public constructors can target httptest servers on the
// loopback interface. outboundSSRFGuard lives in a non-_test file but is only
// flipped here; _test.go files are excluded from production binaries, so the
// guard stays armed in deployment.
func TestMain(m *testing.M) {
	outboundSSRFGuard = false
	os.Exit(m.Run())
}
