package provider

import (
	"testing"
	"time"
)

// M4: the provider-level http_timeout_seconds key — pre-persist validation,
// runtime resolution, and the shared write-path validator aggregation.

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
