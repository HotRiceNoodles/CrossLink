package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/translator"
)

// M0 tests: Anthropic API version support list, the 2026-10-21 sampling
// three-tier rules at the outbound checkpoint, and the top_k carry through
// the internal OpenAI representation.

func newAnthropicForConfig(t *testing.T, extra string) *AnthropicProvider {
	t.Helper()
	p, err := NewAnthropicProvider("test-anthropic", "https://api.example.com", "sk-test", []byte(extra), 5*time.Second)
	if err != nil {
		t.Fatalf("NewAnthropicProvider(%q): %v", extra, err)
	}
	return p
}

func f64p(v float64) *float64 { return &v }
func intp(v int) *int         { return &v }

func TestAnthropicAPIVersionValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"empty config", ``, false},
		{"no api_version", `{"record":false}`, false},
		{"old version", `{"api_version":"2023-06-01"}`, false},
		{"new version", `{"api_version":"2026-10-21"}`, false},
		{"unknown version", `{"api_version":"2024-01-01"}`, true},
		{"garbage string", `{"api_version":"latest"}`, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAnthropicAPIVersion([]byte(tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateAnthropicAPIVersion(%q) error = %v, wantErr %v", tc.extra, err, tc.wantErr)
			}
			// Constructor re-validates as the defense line (M0): rows that
			// slip past pre-persist validation must fail visibly at apply.
			_, err = NewAnthropicProvider("v", "https://api.example.com", "k", []byte(tc.extra), time.Second)
			if (err != nil) != tc.wantErr {
				t.Fatalf("constructor error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestAnthropicSamplingVersionRules(t *testing.T) {
	t.Parallel()
	oldVer := newAnthropicForConfig(t, `{"api_version":"2023-06-01"}`)
	newVer := newAnthropicForConfig(t, `{"api_version":"2026-10-21"}`)
	newStrip := newAnthropicForConfig(t, `{"api_version":"2026-10-21","sampling_compat_strip":true}`)

	baseReq := func() *domain.AnthropicRequest { return &domain.AnthropicRequest{} }

	t.Run("old version is a strict no-op", func(t *testing.T) {
		t.Parallel()
		ar := baseReq()
		ar.Temperature = f64p(0.3)
		ar.TopP = f64p(0.9)
		ar.TopK = intp(40)
		meta := &domain.InboundSamplingMeta{TopK: intp(40)}
		if err := oldVer.applyVersionSamplingRules(context.Background(), ar, meta); err != nil {
			t.Fatalf("old version must not apply rules: %v", err)
		}
		if ar.Temperature == nil || ar.TopP == nil || ar.TopK == nil {
			t.Fatal("old version must not strip or reject sampling params")
		}
	})

	t.Run("no sampling params passes", func(t *testing.T) {
		t.Parallel()
		if err := newVer.applyVersionSamplingRules(context.Background(), baseReq(), nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("default temperature stripped silently", func(t *testing.T) {
		t.Parallel()
		ar := baseReq()
		ar.Temperature = f64p(1.0)
		if err := newVer.applyVersionSamplingRules(context.Background(), ar, nil); err != nil {
			t.Fatalf("default temperature must be stripped, not rejected: %v", err)
		}
		if ar.Temperature != nil {
			t.Fatal("temperature=1.0 should be stripped")
		}
	})

	t.Run("non-default temperature rejected without compat switch", func(t *testing.T) {
		t.Parallel()
		ar := baseReq()
		ar.Temperature = f64p(0.5)
		err := newVer.applyVersionSamplingRules(context.Background(), ar, nil)
		if err == nil {
			t.Fatal("non-default temperature must be rejected")
		}
		var pe *ProviderError
		if !errors.As(err, &pe) || pe.ErrorType != ErrorBadRequest || pe.Param != "temperature" {
			t.Fatalf("expected 400-class ProviderError with param temperature, got %v", err)
		}
		if IsRetryableError(err) {
			t.Fatal("sampling rejection must not be retryable (no fallback churn)")
		}
		if ar.Temperature == nil {
			t.Fatal("a rejected param must not be stripped")
		}
	})

	t.Run("non-default temperature stripped under compat switch", func(t *testing.T) {
		t.Parallel()
		ar := baseReq()
		ar.Temperature = f64p(0.5)
		if err := newStrip.applyVersionSamplingRules(context.Background(), ar, nil); err != nil {
			t.Fatalf("compat strip must not reject: %v", err)
		}
		if ar.Temperature != nil {
			t.Fatal("compat strip should remove temperature")
		}
	})

	t.Run("top_p always non-default tier", func(t *testing.T) {
		t.Parallel()
		// No top_p default is registered in the capability table, so even a
		// value of 1.0 stays in the reject-or-compat-strip tier.
		ar := baseReq()
		ar.TopP = f64p(1.0)
		if err := newVer.applyVersionSamplingRules(context.Background(), ar, nil); err == nil {
			t.Fatal("top_p must be rejected without compat switch")
		}
		ar2 := baseReq()
		ar2.TopP = f64p(1.0)
		if err := newStrip.applyVersionSamplingRules(context.Background(), ar2, nil); err != nil {
			t.Fatalf("top_p with compat switch: %v", err)
		}
		if ar2.TopP != nil {
			t.Fatal("top_p should be stripped under compat switch")
		}
	})

	t.Run("top_k via inbound meta rejected then stripped", func(t *testing.T) {
		t.Parallel()
		// ar.TopK is nil here by construction: the reverse translator cannot
		// produce top_k (the internal representation drops it). Only the
		// InboundSamplingMeta carry knows the client sent top_k — the rule
		// must not be bypassable by that in-flight translation drop.
		meta := &domain.InboundSamplingMeta{TopK: intp(40)}
		if err := newVer.applyVersionSamplingRules(context.Background(), baseReq(), meta); err == nil {
			t.Fatal("top_k carried via meta must be rejected")
		}
		if err := newStrip.applyVersionSamplingRules(context.Background(), baseReq(), meta); err != nil {
			t.Fatalf("top_k with compat switch: %v", err)
		}
	})
}

func TestAnthropicToOpenAICarriesTopKMeta(t *testing.T) {
	t.Parallel()
	req := &domain.AnthropicRequest{
		Model:     "claude-x",
		MaxTokens: 16,
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		TopK:      intp(40),
	}
	oai, err := translator.AnthropicToOpenAI(req, "claude-x")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if oai.InboundSamplingMeta == nil || oai.InboundSamplingMeta.TopK == nil || *oai.InboundSamplingMeta.TopK != 40 {
		t.Fatal("InboundSamplingMeta must carry top_k through translation")
	}

	noTopK := &domain.AnthropicRequest{
		Model:     "claude-x",
		MaxTokens: 16,
		Messages:  []domain.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	}
	oai2, err := translator.AnthropicToOpenAI(noTopK, "claude-x")
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if oai2.InboundSamplingMeta != nil {
		t.Fatal("meta must be nil when top_k is absent")
	}

	// The carry must never leak into any wire encoding (json:"-").
	b, err := json.Marshal(oai)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if _, ok := m["InboundSamplingMeta"]; ok {
			t.Fatal("InboundSamplingMeta must not serialize")
		}
	}
}
