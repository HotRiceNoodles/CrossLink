package guardapp

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/guardrail"
)

// M3 元数据接线：引擎在 Check 过程写入 CheckContext 的产出侧画像
// （AgentType/RiskLevel/ctx.Metadata）以 engine_ 前缀合并进事件 metadata；
// payload 键冲突时优先。
func TestMergedMetadata(t *testing.T) {
	// 无引擎产出：原样 payload
	if got := mergedMetadata(map[string]string{"k": "v"}, nil); string(got) != `{"k":"v"}` {
		t.Fatalf("payload-only: %s", got)
	}
	// 引擎画像 + payload 共存，前缀区分
	ctx := &guardrail.CheckContext{AgentType: "coding_agent", RiskLevel: "high",
		Metadata: map[string]string{"session_seq": "42"}}
	got := mergedMetadata(map[string]string{"user": "alice"}, ctx)
	var m map[string]string
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatal(err)
	}
	if m["engine_agent_type"] != "coding_agent" || m["engine_risk_level"] != "high" ||
		m["engine_session_seq"] != "42" || m["user"] != "alice" {
		t.Fatalf("merged: %v", m)
	}
	if len(m) != 4 {
		t.Fatalf("extra keys: %v", m)
	}
	// 键冲突：payload 赢（引擎不得覆盖客户申报）——ctx 键 agent_type
	// 前缀化后与 payload 键 engine_agent_type 同名，payload 覆盖之。
	ctx2 := &guardrail.CheckContext{Metadata: map[string]string{"agent_type": "spoofed"}}
	got = mergedMetadata(map[string]string{"engine_agent_type": "customer-value"}, ctx2)
	var m2 map[string]string
	_ = json.Unmarshal(got, &m2)
	if m2["engine_agent_type"] != "customer-value" || len(m2) != 1 {
		t.Fatalf("conflict: %v", m2)
	}
	// 双空：nil（repo Create 落空对象）
	if got := mergedMetadata(nil, &guardrail.CheckContext{}); got != nil {
		t.Fatalf("empty: %s", got)
	}
}
