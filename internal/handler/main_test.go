package handler

import (
	"testing"

	"github.com/crosslink/internal/provider"
)

// TestMain disables the production SSRF dialer for this package's tests: the
// acceptance tests point real adapters at httptest loopback upstreams (same
// accommodation internal/service's TestMain makes; no production code touched).
func TestMain(m *testing.M) {
	provider.SetOutboundSSRFGuardForTest(false)
	// 恢复交给进程退出；包内测试均自包含。
	m.Run()
}
