package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/billingintegrity"
	"github.com/gin-gonic/gin"
)

// The admission gate: 503 + Retry-After while blocked, pass-through after
// release (in-flight semantics documented at the middleware).
func TestBillingIntegrityGate_RejectsWhileBlocked(t *testing.T) {
	billingintegrity.ResetForTest()
	t.Cleanup(billingintegrity.ResetForTest)
	billingintegrity.Configure(billingintegrity.PolicyBlock)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(BillingIntegrityGate())
	r.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusOK) })

	// Unblocked: pass-through.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unblocked status = %d, want 200", rec.Code)
	}

	// Blocked: 503 with Retry-After and a typed error body.
	billingintegrity.ReportLoss("test: db+WAL double failure", "evt-test")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("blocked status = %d, want 503", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}
	if rec2.Body.Len() == 0 || !contains(rec2.Body.String(), "billing_integrity_blocked") {
		t.Fatalf("body missing typed error: %q", rec2.Body.String())
	}

	// Released: pass-through again.
	billingintegrity.ResetForTest()
	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("post-release status = %d, want 200", rec3.Code)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
