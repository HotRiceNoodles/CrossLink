package provider

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestErrorQuotaConstant(t *testing.T) {
	if ErrorQuota != "quota" {
		t.Fatalf("ErrorQuota = %q, want %q", ErrorQuota, "quota")
	}
}

func TestParseProviderError_FillsCodeAndType(t *testing.T) {
	body := `{"error":{"message":"no quota","type":"insufficient_quota","code":"insufficient_quota"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
	err := parseProviderError(resp)
	pe, ok := err.(*ProviderError)
	if !ok || pe == nil {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.Code != "insufficient_quota" {
		t.Fatalf("Code = %q, want insufficient_quota", pe.Code)
	}
	if pe.Type != "insufficient_quota" {
		t.Fatalf("Type = %q, want insufficient_quota", pe.Type)
	}
}
