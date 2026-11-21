package handler

import (
	"os"
	"testing"
)

// usageSoakGate env 门控（BEHAVIOR_G0_EVAL=1 同款纪律，2026-11-20 测试分层）：
// usage WAL 的对抗性/吞吐 soak 件以真实 fsync 与真实等待窗运行——Windows 本机
// 上四个 soak 占掉 handler 包 ~170s 墙钟中的 ~168s（fsync 停滞×20 次有界等待
// ~100s、6k/10k 逐行真实 fsync 各 ~27s、持续负载台面 ~8s）。默认全量测试跳过；
// CI soak 作业（Linux，fsync 快）与 `make test-usage-soak` 显式执行，回归覆盖不减。
func usageSoakGate(t *testing.T) {
	t.Helper()
	if os.Getenv("USAGE_SOAK_TEST") != "1" {
		t.Skip("usage WAL soak: set USAGE_SOAK_TEST=1 (or `make test-usage-soak`; CI soak job runs it every push)")
	}
}
