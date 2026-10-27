package guardrail

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type piiVaultKey struct{}

// PIIVault is owned by one HTTP request, never serialized or persisted. A
// random namespace prevents literal/user-supplied placeholders from restoring.
type PIIVault struct {
	mu        sync.Mutex
	namespace string
	values    map[string]string
	tokens    map[string]string
	replacer  *strings.Replacer
	ordered   []string
}

func WithPIIVault(ctx context.Context) (context.Context, *PIIVault, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ctx, nil, err
	}
	// Letter-only randomness cannot itself be mistaken for a phone/card number.
	var namespace strings.Builder
	for _, b := range nonce {
		namespace.WriteByte('a' + b>>4)
		namespace.WriteByte('a' + b&15)
	}
	v := &PIIVault{namespace: namespace.String(), values: map[string]string{}, tokens: map[string]string{}}
	return context.WithValue(ctx, piiVaultKey{}, v), v, nil
}

func PIIVaultFromContext(ctx context.Context) *PIIVault {
	v, _ := ctx.Value(piiVaultKey{}).(*PIIVault)
	return v
}

func (v *PIIVault) Active() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.values) > 0
}

// Clear releases originals even if a background task retains the context.
func (v *PIIVault) Clear() {
	v.mu.Lock()
	defer v.mu.Unlock()
	clear(v.values)
	clear(v.tokens)
	v.replacer = nil
	v.ordered = nil
}

// MaskResult provides an isolated preview for the administrator's rule test.
// It returns placeholders only; the private mapping is never part of result.
func (v *PIIVault) MaskResult(content string, result *GuardrailResult) {
	if result == nil {
		return
	}
	result.MaskedSpans = composeSpans(result.MaskedSpans)
	v.maskSpans(content, result.MaskedSpans)
	if len(result.MaskedSpans) > 0 {
		result.MaskedContent = applySpans(content, result.MaskedSpans)
	}
}

// maskSpans runs only after rule composition, so losing/overlapping spans can
// never reintroduce information another rule deliberately masked.
func (v *PIIVault) maskSpans(content string, spans []Span) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range spans {
		s := &spans[i]
		if !s.Reversible || s.Start < 0 || s.End > len(content) || s.Start >= s.End {
			continue
		}
		original := content[s.Start:s.End]
		key := s.Replacement + "\x00" + original
		token := v.tokens[key]
		if token == "" {
			token = fmt.Sprintf("<CL_PII_%s_%s_%d>", v.namespace, strings.Trim(s.Replacement, "[]"), len(v.values)+1)
			v.tokens[key] = token
			v.values[token] = original
			v.replacer = nil
		}
		s.Replacement = token
	}
}

// RestorePrefix restores complete tokens in one pass (no recursive expansion).
// A possible token suffix is retained for the next delta of the same field.
func (v *PIIVault) RestorePrefix(text string, final bool) (string, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	cut := len(text)
	if v.replacer == nil {
		pairs := make([]string, 0, len(v.values)*2)
		v.ordered = v.ordered[:0]
		for token, original := range v.values {
			pairs = append(pairs, token, original)
			v.ordered = append(v.ordered, token)
		}
		sort.Strings(v.ordered)
		v.replacer = strings.NewReplacer(pairs...)
	}
	if !final {
		if start := strings.LastIndexByte(text, '<'); start >= 0 {
			candidate := text[start:]
			i := sort.SearchStrings(v.ordered, candidate)
			if i < len(v.ordered) && candidate != v.ordered[i] && strings.HasPrefix(v.ordered[i], candidate) {
				cut = start
			}
		}
	}
	return v.replacer.Replace(text[:cut]), text[cut:]
}

func (v *PIIVault) Restore(text string) string {
	result, _ := v.RestorePrefix(text, true)
	return result
}
