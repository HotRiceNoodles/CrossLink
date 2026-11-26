package provider

import (
	"testing"
	"time"
)

// M4: the provider-level http_timeout_seconds key — pre-persist validation,
// runtime resolution, and the shared write-path validator aggregation.
// 图片方案 P1 (design §3.5 评审 F4): the poll_* knobs get the same treatment.

func TestValidateHTTPTimeout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"empty", ``, false},
		{"other keys only", `{"api_version":"2023-06-01"}`, false},
		{"absent via null", `{"http_timeout_seconds":null}`, false},
		{"valid", `{"http_timeout_seconds":60}`, false},
		{"float valid", `{"http_timeout_seconds":30.5}`, false},
		{"lower bound", `{"http_timeout_seconds":5}`, false},
		{"upper bound", `{"http_timeout_seconds":600}`, false},
		{"below range", `{"http_timeout_seconds":4}`, true},
		{"above range", `{"http_timeout_seconds":601}`, true},
		// json.Number accepts a QUOTED number ("60" unquotes into Number) —
		// tolerated: it resolves to the same 60s at runtime. Not a schema
		// violation worth failing a write over.
		{"quoted number tolerated", `{"http_timeout_seconds":"60"}`, false},
		{"wrong type bool rejected", `{"http_timeout_seconds":true}`, true},
		{"malformed json rejected", `{"http_timeout_seconds":`, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateHTTPTimeout([]byte(tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateHTTPTimeout(%q) error = %v, wantErr %v", tc.extra, err, tc.wantErr)
			}
		})
	}
}

func TestResolveHTTPTimeout(t *testing.T) {
	t.Parallel()
	const fallback = 300 * time.Second
	cases := []struct {
		name  string
		extra string
		want  time.Duration
	}{
		{"empty falls back", ``, fallback},
		{"other keys fall back", `{"api_version":"2026-10-21"}`, fallback},
		{"valid value", `{"http_timeout_seconds":60}`, 60 * time.Second},
		{"out of range degrades to fallback", `{"http_timeout_seconds":9000}`, fallback},
		{"wrong type degrades to fallback", `{"http_timeout_seconds":"x"}`, fallback},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveHTTPTimeout([]byte(tc.extra), fallback); got != tc.want {
				t.Fatalf("ResolveHTTPTimeout(%q) = %v, want %v", tc.extra, got, tc.want)
			}
		})
	}
}

func TestValidatePollConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"empty", ``, false},
		{"other keys only", `{"http_timeout_seconds":60}`, false},
		{"absent via null", `{"poll_timeout_seconds":null}`, false},
		{"all three valid", `{"poll_timeout_seconds":300,"poll_interval_ms":2000,"poll_max_interval_ms":8000}`, false},
		{"timeout lower bound", `{"poll_timeout_seconds":10}`, false},
		{"timeout upper bound", `{"poll_timeout_seconds":600}`, false},
		{"timeout below range", `{"poll_timeout_seconds":9}`, true},
		{"timeout above range", `{"poll_timeout_seconds":601}`, true},
		{"interval lower bound", `{"poll_interval_ms":500}`, false},
		{"interval upper bound", `{"poll_interval_ms":30000}`, false},
		{"interval below range", `{"poll_interval_ms":499}`, true},
		{"interval above range", `{"poll_interval_ms":30001}`, true},
		{"max lower bound", `{"poll_max_interval_ms":500}`, false},
		{"max upper bound", `{"poll_max_interval_ms":60000}`, false},
		{"max above range", `{"poll_max_interval_ms":60001}`, true},
		{"max below interval rejected", `{"poll_interval_ms":5000,"poll_max_interval_ms":1000}`, true},
		{"max above interval accepted", `{"poll_interval_ms":5000,"poll_max_interval_ms":6000}`, false},
		// Cross-check only fires when BOTH keys are present: max alone below
		// the default interval is in-range (runtime clamps degenerately).
		{"max alone small tolerated", `{"poll_max_interval_ms":2000}`, false},
		// json.Number tolerates a quoted number, same as http_timeout_seconds.
		{"quoted number tolerated", `{"poll_timeout_seconds":"300"}`, false},
		{"wrong type bool rejected", `{"poll_interval_ms":true}`, true},
		{"malformed json rejected", `{"poll_timeout_seconds":`, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePollConfig([]byte(tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidatePollConfig(%q) error = %v, wantErr %v", tc.extra, err, tc.wantErr)
			}
		})
	}
}

func TestValidateCommonExtraConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"both valid", `{"api_version":"2026-10-21","http_timeout_seconds":60}`, false},
		{"bad version", `{"api_version":"nope","http_timeout_seconds":60}`, true},
		{"bad timeout", `{"api_version":"2023-06-01","http_timeout_seconds":9999}`, true},
		{"both bad reports version first", `{"api_version":"nope","http_timeout_seconds":9999}`, true},
		{"bad poll timeout reaches aggregator", `{"http_timeout_seconds":60,"poll_timeout_seconds":5}`, true},
		{"all keys valid", `{"http_timeout_seconds":60,"poll_timeout_seconds":300,"poll_interval_ms":2000,"poll_max_interval_ms":8000}`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCommonExtraConfig([]byte(tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateCommonExtraConfig(%q) error = %v, wantErr %v", tc.extra, err, tc.wantErr)
			}
		})
	}
}
