package admission

import (
	"strings"
	"testing"
)

// M3.4/F-3 acceptance gap: the admission fast-path cost budget. The fast path
// is one map-unmarshal + linear scan over a typical body with KNOWN params
// only — no candidate resolution, no regex. This benchmark pins its order of
// magnitude; regressions (e.g. someone adding per-key crypto or reflection)
// show up here.
func BenchmarkAdmissionFastPath(b *testing.B) {
	// ~2KB body, known params only (the hot case for every well-formed
	// client: model + messages + sampling + tools).
	msg := strings.Repeat("x", 1800)
	body := `{"model":"gpt-x","max_tokens":1024,"temperature":0.7,"top_p":0.95,` +
		`"messages":[{"role":"user","content":"` + msg + `"}],` +
		`"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]}`
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Admit("openai", []byte(body), ""); err != nil {
			b.Fatalf("fast path must never reject known params: %v", err)
		}
	}
}
