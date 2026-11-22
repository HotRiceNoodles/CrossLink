package service

import (
	"os"
	"testing"

	"github.com/crosslink/internal/provider"
)

// TestMain disables the production SSRF dialer for this package's tests: the
// M3b four-way acceptance matrix drives REAL adapters (openai_compatible /
// anthropic) against httptest servers on the loopback interface — the same
// accommodation internal/provider's own TestMain makes. No production code
// path can reach this (_test.go is excluded from binaries).
func TestMain(m *testing.M) {
	provider.SetOutboundSSRFGuardForTest(false)
	os.Exit(m.Run())
}
