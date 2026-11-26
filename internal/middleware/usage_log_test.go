package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/crosslink/internal/service"
	"github.com/gin-gonic/gin"
	sqlite "github.com/glebarez/sqlite"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

func TestLogRouteTypeFromPath(t *testing.T) {
	tests := []struct {
		pattern string
		want    string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/unknown", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			got := logRouteTypeFromPath(tt.pattern)
			if got != tt.want {
				t.Errorf("logRouteTypeFromPath(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestMapStatusToErrorType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name   string
		status int
		ctxSet map[string]bool
		want   string
	}{
		{"429 rate_limit", 429, nil, "rate_limit"},
		{"429 budget_exceeded", 429, map[string]bool{"budget_exceeded": true}, "budget_exceeded"},
		{"429 call_limit_exceeded", 429, map[string]bool{"call_limit_exceeded": true}, "call_limit_exceeded"},
		{"400", 400, nil, "bad_request"},
		{"401", 401, nil, "auth_failure"},
		{"403", 403, nil, "forbidden"},
		{"404", 404, nil, "not_found"},
		{"503", 503, nil, "service_unavailable"},
		{"500", 500, nil, "server_error"},
		{"502", 502, nil, "server_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tt.ctxSet {
				c.Set(k, v)
			}
			got := mapStatusToErrorType(c, tt.status)
			if got != tt.want {
				t.Errorf("mapStatusToErrorType(_, %d) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestMapStatusToErrorTypePinned(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set("error_type", "invalid_request_error")
	if got := mapStatusToErrorType(c, 400); got != "invalid_request_error" {
		t.Errorf("mapStatusToErrorType with pinned error_type = %q, want invalid_request_error", got)
	}
}

func TestExtractErrMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai envelope", `{"error":{"message":"invalid json","type":"invalid_request_error"}}`, "invalid json"},
		{"anthropic envelope", `{"type":"error","error":{"type":"invalid_request_error","message":"beta token \"context-1m\" is not enabled"}}`, `beta token "context-1m" is not enabled`},
		{"plain error string", `{"error":"invalid severity"}`, "invalid severity"},
		{"guardrail envelope", `{"error":{"type":"guardrail_blocked","message":"blocked by guardrail"}}`, "blocked by guardrail"},
		{"non-json text", "malformed chunked encoding\n", "malformed chunked encoding"},
		{"empty body", "", ""},
		{"error object without message", `{"error":{"code":17}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := extractErrMessage([]byte(tt.body)); got != tt.want {
				t.Errorf("extractErrMessage(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestSanitizeErrMessage(t *testing.T) {
	t.Parallel()
	// Each control char becomes one space (no collapsing — content is preserved).
	if got := sanitizeErrMessage("  a\r\nb\tc  "); got != "a  b c" {
		t.Errorf("sanitizeErrMessage control chars = %q, want %q", got, "a  b c")
	}
	long := strings.Repeat("x", errMessageLimit+100)
	if got := sanitizeErrMessage(long); len(got) != errMessageLimit {
		t.Errorf("sanitizeErrMessage length = %d, want %d", len(got), errMessageLimit)
	}
	// A cut must not split a multi-byte rune.
	cjk := strings.Repeat("桥", 400) // 1200 bytes > 512
	got := sanitizeErrMessage(cjk)
	if !utf8.ValidString(got) && got != "" {
		t.Errorf("sanitizeErrMessage produced invalid UTF-8: %q", got)
	}
	if got == "" {
		t.Errorf("sanitizeErrMessage dropped a non-empty message")
	}
}

func TestErrCaptureWriter(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	// Explicit WriteHeader + Write on the error path captures the body.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ew := &errCaptureWriter{ResponseWriter: c.Writer}
	ew.WriteHeader(http.StatusBadRequest)
	if _, err := ew.Write([]byte(`{"error":{"message":"x"}}`)); err != nil {
		t.Fatal(err)
	}
	if string(ew.buf) != `{"error":{"message":"x"}}` {
		t.Errorf("error body not captured: %q", ew.buf)
	}

	// Success responses buffer nothing.
	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	ew2 := &errCaptureWriter{ResponseWriter: c2.Writer}
	ew2.WriteHeader(http.StatusOK)
	if _, err := ew2.Write([]byte(" streamed chunk ")); err != nil {
		t.Fatal(err)
	}
	if len(ew2.buf) != 0 {
		t.Errorf("success body captured: %q", ew2.buf)
	}

	// The c.JSON path: c.Status → WriteHeader → WriteHeaderNow.
	w3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(w3)
	ew3 := &errCaptureWriter{ResponseWriter: c3.Writer}
	c3.Writer = ew3
	c3.Status(http.StatusForbidden)
	if _, err := ew3.WriteString("no"); err != nil {
		t.Fatal(err)
	}
	if !ew3.capture || len(ew3.buf) != 2 {
		t.Errorf("WriteHeaderNow path not captured: capture=%v buf=%q", ew3.capture, ew3.buf)
	}

	// Oversized bodies are capped, never unbounded.
	w4 := httptest.NewRecorder()
	c4, _ := gin.CreateTestContext(w4)
	ew4 := &errCaptureWriter{ResponseWriter: c4.Writer}
	ew4.WriteHeader(http.StatusBadRequest)
	if _, err := ew4.Write(make([]byte, errBodyCaptureLimit*3)); err != nil {
		t.Fatal(err)
	}
	if len(ew4.buf) != errBodyCaptureLimit {
		t.Errorf("capture cap = %d, want %d", len(ew4.buf), errBodyCaptureLimit)
	}
}

// TestUsageLogGatewayRejectionDetail drives the full fallback row: a
// gateway-side 400 must land in usage_logs with the extracted message, the
// pinned error_type/stage, and the guardrail context passthrough.
func TestUsageLogGatewayRejectionDetail(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	newSvc := func(t *testing.T) (*service.UsageService, *gorm.DB) {
		t.Helper()
		db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: glogger.Default.LogMode(glogger.Silent)})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
			t.Fatal(err)
		}
		// ":memory:" is per-connection: pin the pool to ONE connection so the
		// background logging goroutine and the test query see the same DB.
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.SetMaxOpenConns(1)
		}
		return service.NewUsageService(repository.NewUsageLogRepo(db)), db
	}

	t.Run("admission rejection", func(t *testing.T) {
		t.Parallel()
		svc, db := newSvc(t)
		r := gin.New()
		r.Use(UsageLog(svc))
		r.POST("/v1/messages", func(c *gin.Context) {
			c.Set("error_type", "invalid_request_error")
			c.Set("error_stage", "admission")
			c.JSON(http.StatusBadRequest, gin.H{
				"type":  "error",
				"error": gin.H{"type": "invalid_request_error", "message": `beta token "context-1m" is not enabled on this gateway`},
			})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", w.Code)
		}
		row := waitForUsageRow(t, db)
		if row.ErrorType != "invalid_request_error" {
			t.Errorf("ErrorType = %q, want invalid_request_error", row.ErrorType)
		}
		if row.ErrorStage == nil || *row.ErrorStage != "admission" {
			t.Errorf("ErrorStage = %v, want admission", row.ErrorStage)
		}
		if row.ErrorMessage == nil || !strings.Contains(*row.ErrorMessage, "context-1m") {
			t.Errorf("ErrorMessage = %v, want beta-token detail", row.ErrorMessage)
		}
		if row.RouteType != "anthropic" {
			t.Errorf("RouteType = %q, want anthropic", row.RouteType)
		}
		if row.UpstreamStatus != nil {
			t.Errorf("UpstreamStatus = %v, want nil (gateway-side)", row.UpstreamStatus)
		}
	})

	t.Run("guardrail block", func(t *testing.T) {
		t.Parallel()
		svc, db := newSvc(t)
		r := gin.New()
		r.Use(UsageLog(svc))
		r.POST("/v1/chat/completions", func(c *gin.Context) {
			c.Set("guardrail_triggered", true)
			c.Set("guardrail_rule", "prompt-injection")
			c.Set("security_events", datatypes.JSON([]byte(`[{"rule":"prompt-injection","action":"block","reason":"matched injection pattern"}]`)))
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{"type": "guardrail_blocked", "message": "blocked by guardrail"},
			})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", w.Code)
		}
		row := waitForUsageRow(t, db)
		if row.ErrorType != "bad_request" {
			t.Errorf("ErrorType = %q, want bad_request", row.ErrorType)
		}
		if row.ErrorStage == nil || *row.ErrorStage != "guardrail" {
			t.Errorf("ErrorStage = %v, want guardrail (derived)", row.ErrorStage)
		}
		if !row.GuardrailTriggered || row.GuardrailRule != "prompt-injection" {
			t.Errorf("guardrail context = %v/%q", row.GuardrailTriggered, row.GuardrailRule)
		}
		if !strings.Contains(string(row.SecurityEvents), "prompt-injection") {
			t.Errorf("SecurityEvents = %s, want rule detail", row.SecurityEvents)
		}
		if row.ErrorMessage == nil || *row.ErrorMessage != "blocked by guardrail" {
			t.Errorf("ErrorMessage = %v, want captured body message", row.ErrorMessage)
		}
	})

	t.Run("bare 400 fallback", func(t *testing.T) {
		t.Parallel()
		svc, db := newSvc(t)
		r := gin.New()
		r.Use(UsageLog(svc))
		r.POST("/v1/messages", func(c *gin.Context) {
			c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": "request too large"}})
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
		row := waitForUsageRow(t, db)
		if row.ErrorType != "bad_request" {
			t.Errorf("ErrorType = %q, want bad_request", row.ErrorType)
		}
		if row.ErrorStage != nil {
			t.Errorf("ErrorStage = %v, want nil", row.ErrorStage)
		}
		if row.ErrorMessage == nil || *row.ErrorMessage != "request too large" {
			t.Errorf("ErrorMessage = %v, want request too large", row.ErrorMessage)
		}
	})
}

// waitForUsageRow polls until the async fallback row lands (bounded logging
// goroutine + 5s write budget).
func waitForUsageRow(t *testing.T, db *gorm.DB) model.UsageLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var rows []model.UsageLog
		if err := db.Order("id DESC").Limit(1).Find(&rows).Error; err != nil {
			t.Fatalf("query usage row: %v", err)
		}
		if len(rows) == 1 {
			return rows[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("usage fallback row not written within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
