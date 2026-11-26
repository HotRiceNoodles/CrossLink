package admission

import (
	"encoding/json"
	"testing"

	"github.com/crosslink/internal/domain"
)

// M3 first-slice admission tests: the M3.2 priority table's tiering on real
// raw bodies — billing-coupled rejection, structured-unknown rejection,
// scalar-unknown observe-drop (pass), known pass, beta-token whitelist.

func TestAdmit_Anthropic(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		beta      string
		wantErr   bool
		wantParam string
	}{
		{"known params pass", `{"model":"claude-x","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_k":40}`, "", false, ""},
		{"billing-coupled speed rejected", `{"model":"m","speed":"fast"}`, "", true, "speed"},
		{"service_tier rejected", `{"model":"m","service_tier":"auto"}`, "", true, "service_tier"},
		// Phase-1: effort rides the envelope; Phase-2: format (structured
		// outputs) admitted as an object. task_budget and bad shapes stay
		// rejected.
		{"output_config.task_budget still rejected", `{"model":"m","output_config":{"task_budget":{"total":64000}}}`, "", true, "output_config.task_budget"},
		{"output_config scalar format rejected", `{"model":"m","output_config":{"format":"json"}}`, "", true, "output_config.format"},
		{"output_config bad enum rejected", `{"model":"m","output_config":{"effort":"ultra"}}`, "", true, "output_config.effort"},
		{"output_config non-string effort rejected", `{"model":"m","output_config":{"effort":3}}`, "", true, "output_config.effort"},
		{"request-level cache_control rejected", `{"model":"m","cache_control":{"type":"ephemeral"}}`, "", true, "cache_control"},
		{"unknown scalar observe-drops", `{"model":"m","verbosity":"low"}`, "", false, ""},
		{"unknown object rejected", `{"model":"m","new_tool":{"type":"x"}}`, "", true, "new_tool"},
		{"unknown array rejected", `{"model":"m","attachments":[1,2]}`, "", true, "attachments"},
		{"beta token rejected (empty whitelist)", `{"model":"m"}`, "fast-mode-2026-02-01", true, "fast-mode-2026-02-01"},
		// Phase-2 三件套齐后 compact 开白名单；未登记 token 仍拒。
		{"beta token rejected (unregistered)", `{"model":"m"}`, "context-management-2025-06-27", true, "context-management-2025-06-27"},
		{"empty beta header passes", `{"model":"m"}`, "  ", false, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, err := Admit("anthropic", []byte(tc.body), tc.beta)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection, got envelope=%v", env)
				}
				ae, ok := err.(*AdmissionError)
				if !ok {
					t.Fatalf("expected *AdmissionError, got %T", err)
				}
				if ae.Param != tc.wantParam {
					t.Fatalf("param = %q, want %q", ae.Param, tc.wantParam)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAdmit_OpenAI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		body      string
		wantErr   bool
		wantParam string
	}{
		{"known params pass", `{"model":"gpt","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, false, ""},
		{"web_search_options rejected", `{"model":"gpt","web_search_options":{}}`, true, "web_search_options"},
		{"prediction rejected", `{"model":"gpt","prediction":{"content":"x"}}`, true, "prediction"},
		{"audio rejected", `{"model":"gpt","audio":{"voice":"alloy"}}`, true, "audio"},
		{"unknown scalar observe-drops", `{"model":"gpt","chat_prompt_cache_key":"k1"}`, false, ""},
		{"unknown object rejected", `{"model":"gpt","future_tool":{"a":1}}`, true, "future_tool"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Admit("openai", []byte(tc.body), "")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected rejection")
				}
				if ae := err.(*AdmissionError); ae.Param != tc.wantParam {
					t.Fatalf("param = %q, want %q", ae.Param, tc.wantParam)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestAdmit_MalformedBodyPassesThrough(t *testing.T) {
	t.Parallel()
	env, err := Admit("anthropic", []byte(`not json`), "")
	if err != nil || env != nil {
		t.Fatalf("malformed body must pass through to the struct gate, got env=%v err=%v", env, err)
	}
}

// TestAdmit_OutputConfigEffortCarried (Phase-1): every admitted enum value
// rides the envelope under anthropic.output_config, verbatim.
func TestAdmit_OutputConfigEffortCarried(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max"} {
		body := `{"model":"m","output_config":{"effort":"` + effort + `"}}`
		env, err := Admit("anthropic", []byte(body), "")
		if err != nil {
			t.Fatalf("effort %q: unexpected rejection: %v", effort, err)
		}
		if env == nil || env.Params == nil {
			t.Fatalf("effort %q: expected envelope params, got %v", effort, env)
		}
		raw, ok := env.Params["anthropic.output_config"]
		if !ok {
			t.Fatalf("effort %q: anthropic.output_config missing from envelope", effort)
		}
		var got struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(raw, &got); err != nil || got.Effort != effort {
			t.Fatalf("effort %q: envelope carries %s", effort, raw)
		}
	}
	// The openai table must NOT register output_config: an openai client
	// sending the object falls to the structured-unknown rule.
	if _, err := Admit("openai", []byte(`{"model":"g","output_config":{"effort":"high"}}`), ""); err == nil {
		t.Fatal("openai source must not admit output_config")
	}
}

// TestAdmit_OutputConfigFormatCarried (Phase-2): format rides the envelope
// verbatim and coexists with effort.
func TestAdmit_OutputConfigFormatCarried(t *testing.T) {
	t.Parallel()
	formatJSON := `{"type":"json_schema","json_schema":{"name":"out","strict":true}}`
	body := `{"model":"m","output_config":{"effort":"high","format":` + formatJSON + `}}`
	env, err := Admit("anthropic", []byte(body), "")
	if err != nil {
		t.Fatalf("format+effort must pass: %v", err)
	}
	raw := env.Params["anthropic.output_config"]
	var got struct {
		Effort string          `json:"effort"`
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Effort != "high" {
		t.Fatalf("envelope carries %s", raw)
	}
	var f map[string]any
	if err := json.Unmarshal(got.Format, &f); err != nil || f["type"] != "json_schema" {
		t.Fatalf("format must ride verbatim, got %s", got.Format)
	}
}

// TestAdmit_WhitelistedBetaTokenCarried (Phase-2 §4): a whitelisted token
// rides the envelope Headers for the outbound half (provider setBetaHeader).
func TestAdmit_WhitelistedBetaTokenCarried(t *testing.T) {
	t.Parallel()
	env, err := Admit("anthropic", []byte(`{"model":"m"}`), "thinking-display-updates-2026-08-18")
	if err != nil {
		t.Fatalf("whitelisted token must pass: %v", err)
	}
	if env == nil || env.Headers["anthropic-beta"] != "thinking-display-updates-2026-08-18" {
		t.Fatalf("envelope must carry the deduped header, got %+v", env)
	}
	// Mixed whitelisted + unknown: rejected on the unknown one.
	if _, err := Admit("anthropic", []byte(`{"model":"m"}`), "thinking-display-updates-2026-08-18, fast-mode-2026-02-01"); err == nil {
		t.Fatal("mixed header with an unknown token must reject")
	}
}

func TestProtocolExtensionsClone(t *testing.T) {
	t.Parallel()
	env := &domain.ProtocolExtensions{
		SourceProtocol: "anthropic",
		Params:         map[string]json.RawMessage{"sampling.top_k": json.RawMessage(`40`)},
		Headers:        map[string]string{"anthropic-beta": "x"},
	}
	cp := env.Clone()
	cp.Params["sampling.top_k"] = json.RawMessage(`999`)
	cp.Headers["anthropic-beta"] = "y"
	if string(env.Params["sampling.top_k"]) != "40" || env.Headers["anthropic-beta"] != "x" {
		t.Fatal("Clone must deep-copy maps — mutating the copy must not leak into the original")
	}
	var nilEnv *domain.ProtocolExtensions
	if nilEnv.Clone() != nil {
		t.Fatal("nil envelope clones to nil")
	}
}
