package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// Regression tests for a user-visible misreport: a base_url pasted with
// surrounding whitespace (typical when copying a URL out of a chat window or
// document) passed isValidProviderURL — which trims internally — but failed
// guardrail.ValidateURLSafetyDetail, which parsed the RAW string. Every
// failure of the detail validator, including pure parse errors, was then
// reported by all entry points as "must not point to an internal address",
// with no allowlist admission record. The user sees an allowlisted address
// being "rejected as internal" while the real cause is an invisible trailing
// space.

// probeWith runs the onboarding probe entry with the given base_url and
// returns (status, body).
func probeWith(t *testing.T, baseURL string) (int, string) {
	t.Helper()
	h := newOnboardingHandlerForTest(setupOnboardingTestDB(t))
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers/probe", gin.H{
		"adapter_type": "openai_compatible",
		"base_url":     baseURL,
		"api_key":      "sk-test",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Probe(c)
	return w.Code, w.Body.String()
}

func TestOnboardingProbe_SurroundingWhitespace_URLPolicy(t *testing.T) {
	for name, url := range map[string]string{
		"trailing space":     "http://10.99.0.7:11434/v1 ",
		"trailing newline":   "http://10.99.0.7:11434/v1\n",
		"trailing CR/LF":     "http://10.99.0.7:11434/v1\r\n",
		"leading space":      " http://10.99.0.7:11434/v1",
		"wrapped in spaces":  "  http://10.99.0.7:11434/v1  ",
		"trailing fullwidth": "http://10.99.0.7:11434/v1\u3000",
	} {
		t.Run(name, func(t *testing.T) {
			withAllowlist(t, "10.99.0.0/16")
			// The trimmed URL is allowlisted, so the probe must move past
			// SSRF admission exactly as it does for the clean form — the
			// whitespace copy artifact must not turn into an "internal
			// address" rejection.
			code, body := probeWith(t, url)
			assert.NotEqual(t, http.StatusBadRequest, code, "body: %s", body)
		})
	}
}

func TestOnboardingProbe_MalformedURL_TruthfulMessage(t *testing.T) {
	withAllowlist(t, "10.99.0.0/16")

	// A stray double colon is NOT an internal address. It must be reported
	// as an invalid URL, not as an SSRF hit. (A full-width colon from a
	// Chinese IME parses as a hostname and fails DNS instead — see below; Go
	// accepts out-of-range ports syntactically, so those flow into the
	// normal host policy and fail later as a truthful dial error.)
	for name, url := range map[string]string{
		"double colon": "http://10.99.0.7::11434/v1",
	} {
		t.Run(name, func(t *testing.T) {
			code, body := probeWith(t, url)
			assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
			assert.Contains(t, body, "not a valid URL")
			assert.NotContains(t, body, "internal address", "parse errors must not be misreported as SSRF rejections: %s", body)
		})
	}
}

func TestOnboardingProbe_FullwidthColon_ResolveFailure(t *testing.T) {
	withAllowlist(t, "10.99.0.0/16")
	// A full-width colon (Chinese IME) is a valid URL byte to net/url, so the
	// whole host "10.99.0.7：11434" goes to DNS, fails, and must be reported
	// as unresolvable — carrying the host back so the operator can SEE the
	// full-width character — never as an internal-address rejection.
	code, body := probeWith(t, "http://10.99.0.7：11434/v1")
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "could not be resolved")
	assert.Contains(t, body, "10.99.0.7：11434")
	assert.NotContains(t, body, "internal address", "body: %s", body)
}

func TestOnboardingProbe_RestrictedTarget_MessageCarriesAddress(t *testing.T) {
	// Strict default (no allowlist): the rejection stays, but the message
	// must carry the actual address so the operator can tell it apart from
	// a malformed URL.
	code, body := probeWith(t, "http://10.99.0.7:11434/v1")
	assert.Equal(t, http.StatusBadRequest, code, "body: %s", body)
	assert.Contains(t, body, "internal address")
	assert.Contains(t, body, "10.99.0.7")
}

func TestProviderCreate_TrailingWhitespace_IsStoredTrimmed(t *testing.T) {
	// The stored provider BaseURL must be the trimmed value: otherwise a
	// probe would pass admission while every later dial fails on the raw
	// stored URL.
	var stored string
	repo := &mockProviderRepo{
		getByIDFn: func(_ context.Context, _, _ int64) (*model.Provider, error) {
			return &model.Provider{ID: 7, Name: "p", DisplayName: "p", AdapterType: "openai_compatible", BaseURL: "https://api.openai.com/v1", Status: 1}, nil
		},
		createFn: func(_ context.Context, p *model.Provider) error {
			stored = p.BaseURL
			return nil
		},
		updateFn: func(_ context.Context, _ *model.Provider) error { return nil },
	}
	h := NewProviderHandler(repo, &mockProviderModelRepo{
		countByProviderIDFn: func(_ context.Context, _ int64) (int64, error) { return 0, nil },
	}, noOpCache{}, provider.NewRegistry(), nil, nil, nil, nil, nil, nil)

	withAllowlist(t, "10.99.0.0/16")
	c, w := newTestContext(t, http.MethodPost, "/admin/api/providers", gin.H{
		"name": "onprem", "display_name": "On-Prem", "adapter_type": "openai_compatible",
		"base_url": " http://10.99.0.7:11434/v1\r\n", "api_key": "sk-x",
	})
	setAdminContext(c, 1, 1, "admin")
	h.Create(c)
	assert.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.True(t, !strings.HasPrefix(stored, " ") && !strings.HasSuffix(stored, "\n"),
		"stored BaseURL must be trimmed, got %q", stored)
	assert.Equal(t, "http://10.99.0.7:11434/v1", stored)
}
