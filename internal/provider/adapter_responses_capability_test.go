package provider

import (
	"testing"
	"time"

	"github.com/crosslink/internal/model"
)

// supports_responses capability gate (write side): the flag must be
// configurable exactly where the runtime 3A double gate can honor it.
func TestAdapterServesResponses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		adapter string
		extra   string
		want    bool
	}{
		{"native openai_compatible", "openai_compatible", ``, true},
		{"ollama (same impl)", "ollama", ``, true},
		{"native anthropic", "anthropic", ``, false},
		{"anthropic with stray protocol key", "anthropic", `{"api_protocol":"openai"}`, false}, // native adapter ignores the key; explicit type check first
		{"azure (registered, no capability, no switch)", "azure_openai", ``, false},
		{"shell anthropic mode", "shell-fixture", `{"api_protocol":"anthropic"}`, false},
		{"shell openai mode", "shell-fixture", `{"api_protocol":"openai"}`, true},
		{"shell default (openai)", "shell-fixture", ``, true},
		{"unregistered overlay shell under community build", "some-overlay-shell", ``, true}, // runtime gate decides
	}
	// Register a protocol-switched shell fixture (mirrors overlay cloud shells).
	RegisterAdapter("shell-fixture", func(p *model.Provider, timeout time.Duration) (Provider, error) {
		return NewOpenAICompatible(p.Name, p.BaseURL, timeout), nil
	}, &AdapterMeta{
		DisplayName: "Shell Fixture", Capabilities: []string{"chat", "stream"},
		ExtraFields: []AdapterField{{Name: "api_protocol", Type: "select"}},
	})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := AdapterServesResponses(tc.adapter, []byte(tc.extra)); got != tc.want {
				t.Fatalf("AdapterServesResponses(%q, %q) = %v, want %v", tc.adapter, tc.extra, got, tc.want)
			}
		})
	}
}
