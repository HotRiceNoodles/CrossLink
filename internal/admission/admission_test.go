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
		{"output_config rejected", `{"model":"m","output_config":{"effort":"high"}}`, "", true, "output_config"},
		{"request-level cache_control rejected", `{"model":"m","cache_control":{"type":"ephemeral"}}`, "", true, "cache_control"},
		{"unknown scalar observe-drops", `{"model":"m","verbosity":"low"}`, "", false, ""},
		{"unknown object rejected", `{"model":"m","new_tool":{"type":"x"}}`, "", true, "new_tool"},
		{"unknown array rejected", `{"model":"m","attachments":[1,2]}`, "", true, "attachments"},
		{"beta token rejected (empty whitelist)", `{"model":"m"}`, "fast-mode-2026-02-01", true, "fast-mode-2026-02-01"},
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
