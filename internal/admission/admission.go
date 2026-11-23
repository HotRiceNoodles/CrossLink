// Package admission holds the M3 request-admission core: classification of
// raw inbound protocol parameters per the M3.2 priority table (revision-4
// F-1 tiering). It lives outside handler/ so the pre-cache middleware
// (middleware/admission.go, design §2) can run it WITHOUT importing the
// handler package — handlers re-use the same result from the request context.
//
// See docs/design/protocol-envelope-and-admission.md and
// docs/plans/2026-11-21-provider-model-registration-improvement.md (M3.1–M3.4).
package admission

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crosslink/internal/domain"
)

// AdmissionError is a stable, client-facing rejection. Param names the field
// (or the beta token); Reason is a machine-readable category.
type AdmissionError struct {
	Param  string
	Reason string
}

func (e *AdmissionError) Error() string {
	switch e.Reason {
	case "beta_token_not_allowed":
		return fmt.Sprintf("beta token %q is not enabled on this gateway; remove the anthropic-beta header or contact the operator", e.Param)
	default:
		return fmt.Sprintf("unsupported parameter %q: not enabled on this gateway; remove it from the request", e.Param)
	}
}

// billingCoupledParams lists params rejected outright in the first period
// (plan M2 item 4): each carries unverified pricing, server-side state, or
// unverified semantics. Registered rule tables (design §3) replace these sets
// as fields are verified.
var billingCoupledParams = map[string]map[string]bool{
	"anthropic": {
		"speed":         true, // fast mode: independent pricing + usage.speed settlement
		"service_tier":  true, // priority capacity billing
		"container":     true, // server-side container state
		"inference_geo": true, // data-residency coupling
		"diagnostics":   true, // prior-request server state
		"cache_control": true, // request-level cache billing
		"output_config": true, // effort semantics unverified until registered
		"mcp_servers":   true, // server-side MCP connector: upstream-side tool execution + state
	},
	"openai": {
		"web_search_options": true, // introduces tool execution + billing
		"prediction":         true, // carries content
		"audio":              true, // multimodal execution
		"modalities":         true, // multimodal execution
	},
}

// knownTopLevelParams mirrors the request structs — everything the struct gate
// already parses. Unknown-but-scalar keys outside these sets observe-drop.
var knownTopLevelParams = map[string]map[string]bool{
	"anthropic": {
		"model": true, "max_tokens": true, "messages": true, "system": true,
		"stream": true, "temperature": true, "top_p": true, "top_k": true,
		"stop_sequences": true, "metadata": true, "tools": true,
		"tool_choice": true, "thinking": true,
	},
	"openai": {
		"model": true, "messages": true, "max_tokens": true,
		"max_completion_tokens": true, "n": true, "stream": true,
		"temperature": true, "top_p": true, "stop": true, "user": true,
		"stream_options": true, "tools": true, "tool_choice": true,
		"reasoning_effort": true, "thinking": true,
		// Wire-native generation params (struct fields — openai→openai
		// passthrough). service_tier stays OFF this list on purpose:
		// priority processing is billing-coupled.
		"response_format": true, "frequency_penalty": true,
		"presence_penalty": true, "seed": true,
		"parallel_tool_calls": true, "logprobs": true,
		"top_logprobs": true, "logit_bias": true, "store": true,
	},
}

// allowedBetaTokens is the code-level whitelist for billing/state-coupled
// tokens (M2 dual track). It ships EMPTY: no token has completed the
// verification checklist (request/response structure, stream events,
// permissions, billing), so none is admitted. Entries land here only together
// with their verification record; purely-semantic tokens move to the
// settings-configured track later — also not before verification.
var allowedBetaTokens = map[string]bool{}

// extensionParamRule registers an ADMISSIBLE dialect parameter (M3.2 first
// batch). A registered param is typed-validated and CARRIED on the envelope
// (Params) instead of observe-dropped; whether it is actually FORWARDED to a
// given upstream is decided at execution time by the route's
// allowed_extensions / request_overrides opt-in — cost-bearing dialect params
// must not reach an upstream without that model's consent.
type extensionParamRule struct {
	// EnvelopeKey is the namespaced Params key the value rides under.
	EnvelopeKey string
	// ValueType is the required JSON shape: "bool" | "number" | "string".
	ValueType string
}

var extensionParamRules = map[string]map[string]extensionParamRule{
	"openai": {
		// Qwen3 thinking switch: scalar bool, affects OUTPUT TOKEN VOLUME
		// (thinking is billed as output) — forwarded only on opted-in routes.
		"enable_thinking": {EnvelopeKey: "dialect.enable_thinking", ValueType: "bool"},
	},
}

// DialectWireName maps an envelope Params key back to its upstream wire name
// for the serialization-side merge; ok=false for keys without a wire form.
func DialectWireName(envelopeKey string) (string, bool) {
	for _, rules := range extensionParamRules {
		for wire, r := range rules {
			if r.EnvelopeKey == envelopeKey {
				return wire, true
			}
		}
	}
	return "", false
}

// Admit classifies the raw body + beta header per the priority table and
// returns the envelope to attach to the request. A nil envelope with a nil
// error means "nothing to carry" — callers treat that as pass-through.
func Admit(source string, rawBody []byte, betaHeader string) (*domain.ProtocolExtensions, error) {
	var top map[string]json.RawMessage
	if len(rawBody) > 0 && json.Unmarshal(rawBody, &top) != nil {
		// Not a JSON object at top level: the struct unmarshal downstream
		// owns reporting malformed bodies.
		return nil, nil
	}
	rejected := billingCoupledParams[source]
	known := knownTopLevelParams[source]
	extRules := extensionParamRules[source]
	var env *domain.ProtocolExtensions
	for k, v := range top {
		if rejected[k] {
			return nil, &AdmissionError{Param: k, Reason: "unsupported_parameter"}
		}
		if known[k] {
			continue
		}
		if rule, ok := extRules[k]; ok {
			// Registered dialect param: typed-validated and carried on the
			// envelope — forwarding is gated per-route at execution.
			if err := checkExtensionShape(k, v, rule.ValueType); err != nil {
				return nil, err
			}
			if env == nil {
				env = &domain.ProtocolExtensions{SourceProtocol: source}
			}
			if env.Params == nil {
				env.Params = make(map[string]json.RawMessage)
			}
			env.Params[rule.EnvelopeKey] = v
			continue
		}
		if isStructuredJSON(v) {
			return nil, &AdmissionError{Param: k, Reason: "unsupported_parameter"}
		}
		// Unknown scalar: observe-drop (identical outcome to the struct gate;
		// logged so operators see what clients are sending).
		slog.Info("admission: observe-dropped unknown scalar param",
			"protocol", source, "param", k)
	}

	if source == "anthropic" && strings.TrimSpace(betaHeader) != "" {
		tokens := ParseBetaTokens(betaHeader)
		for _, tok := range tokens {
			if !allowedBetaTokens[tok] {
				return nil, &AdmissionError{Param: tok, Reason: "beta_token_not_allowed"}
			}
		}
		// All tokens whitelisted (not reachable while the whitelist is empty,
		// but the plumbing is the deliverable): record the deduped header.
		if env == nil {
			env = &domain.ProtocolExtensions{SourceProtocol: source}
		}
		env.Headers = map[string]string{"anthropic-beta": strings.Join(tokens, ",")}
	}
	return env, nil
}

// checkExtensionShape validates a registered dialect param's JSON shape
// before it rides the envelope.
func checkExtensionShape(param string, v json.RawMessage, want string) error {
	switch want {
	case "bool":
		var b bool
		if json.Unmarshal(v, &b) == nil {
			return nil
		}
	case "number":
		var f float64
		if json.Unmarshal(v, &f) == nil {
			return nil
		}
	case "string":
		var s string
		if json.Unmarshal(v, &s) == nil {
			return nil
		}
	}
	return &AdmissionError{Param: param, Reason: "unsupported_parameter"}
}

// ParseBetaTokens splits and dedupes an anthropic-beta header value.
func ParseBetaTokens(header string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, tok := range strings.Split(header, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out
}

// isStructuredJSON reports whether the raw value is an object or array — the
// shape tier that can introduce execution/billing semantics (F-1 tiering).
func isStructuredJSON(v json.RawMessage) bool {
	for _, b := range v {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}
