package provider

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crosslink/internal/domain"
)

// DeepSeek-incident fix (2026-11-23): sampling_policy decouples official
// semantics from the wire version. "pass" is the explicit opt-out for
// Anthropic-COMPATIBLE endpoints (DeepSeek /anthropic, cloud shells) that
// never adopted the official sampling removal; the default stays "official"
// so real-API users keep the fail-fast protection.

func TestSamplingPolicy_PassThroughCompatibles(t *testing.T) {
	t.Parallel()
	p, err := NewAnthropicProvider("deepseek-compat", "https://api.deepseek.com/anthropic", "k",
		[]byte(`{"api_version":"2026-10-21","sampling_policy":"pass"}`), 5e9)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}

	// The exact incident shape: non-default temperature must PASS (the
	// upstream accepts it; the gateway must not be stricter).
	ar := &domain.AnthropicRequest{Temperature: f64p(0.7)}
	if err := p.applyVersionSamplingRules(context.Background(), ar, nil); err != nil {
		t.Fatalf("sampling_policy=pass must not reject: %v", err)
	}
	if ar.Temperature == nil || *ar.Temperature != 0.7 {
		t.Fatal("pass mode must leave the parameter untouched on the wire")
	}

	// top_p / top_k likewise.
	ar2 := &domain.AnthropicRequest{TopP: f64p(0.9), TopK: intp(40)}
	if err := p.applyVersionSamplingRules(context.Background(), ar2, &domain.InboundSamplingMeta{TopK: intp(40)}); err != nil {
		t.Fatalf("pass mode top_p/top_k: %v", err)
	}
	if ar2.TopP == nil || ar2.TopK != nil && *ar2.TopK != 40 {
		t.Fatal("pass mode must not strip")
	}
}

func TestSamplingPolicy_DefaultOfficialStillProtects(t *testing.T) {
	t.Parallel()
	// No sampling_policy → official semantics (real-API users keep the
	// fail-fast tier-2 rejection; back-compat with the M0 contract).
	p, err := NewAnthropicProvider("real-anthropic", "https://api.anthropic.com", "k",
		[]byte(`{"api_version":"2026-10-21"}`), 5e9)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	err = p.applyVersionSamplingRules(context.Background(), &domain.AnthropicRequest{Temperature: f64p(0.7)}, nil)
	if err == nil {
		t.Fatal("default policy must keep rejecting non-default sampling")
	}
	// The error now names the third way out for compatible endpoints.
	if !strings.Contains(err.Error(), "sampling_policy=pass") {
		t.Fatalf("error must mention the compatible-endpoint escape hatch: %v", err)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) || !IsRetryableError(err) == false {
		t.Fatal("rejection must stay non-retryable")
	}
}

func TestSamplingPolicy_ExplicitOfficialAndBogus(t *testing.T) {
	t.Parallel()
	if _, err := NewAnthropicProvider("x", "https://x", "k", []byte(`{"api_version":"2026-10-21","sampling_policy":"official"}`), 5e9); err != nil {
		t.Fatalf("explicit official must be accepted: %v", err)
	}
	if _, err := NewAnthropicProvider("x", "https://x", "k", []byte(`{"sampling_policy":"lenient"}`), 5e9); err == nil {
		t.Fatal("bogus policy value must fail construction")
	}
}
