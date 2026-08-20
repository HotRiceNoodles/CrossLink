package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/service"
)

type fakeAuditLogger struct {
	entries []*model.AuditLog
}

func (f *fakeAuditLogger) Log(entry *model.AuditLog) {
	f.entries = append(f.entries, entry)
}

func patAuditReq(t *testing.T, audit *fakeAuditLogger, patID any, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if audit != nil {
		r.Use(PATAudit(audit))
	} else {
		r.Use(PATAudit(nil))
	}
	if patID != nil {
		r.Use(func(c *gin.Context) {
			c.Set("pat_id", patID)
			c.Set("user_id", int64(7))
			c.Set("username", "agent")
			c.Next()
		})
	}
	r.GET("/admin/api/pat/keys", handler)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/pat/keys?secret=1", nil)
	req.Header.Set("User-Agent", "pat-agent/1.0")
	r.ServeHTTP(w, req)
	return w
}

func TestPATAudit_Success(t *testing.T) {
	audit := &fakeAuditLogger{}
	w := patAuditReq(t, audit, int64(42), func(c *gin.Context) { c.Status(http.StatusOK) })
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Action != "pat:request" {
		t.Errorf("Action = %q", e.Action)
	}
	if e.ResourceType != "pat" || e.ResourceID != "42" {
		t.Errorf("resource = %q/%q", e.ResourceType, e.ResourceID)
	}
	if e.UserID != 7 || e.Username != "agent" {
		t.Errorf("identity = %v/%q", e.UserID, e.Username)
	}
	if e.Status != "success" {
		t.Errorf("Status = %q", e.Status)
	}
	if e.IPAddress == "" || e.UserAgent != "pat-agent/1.0" {
		t.Errorf("ip/ua = %q/%q", e.IPAddress, e.UserAgent)
	}
	var d map[string]any
	if err := json.Unmarshal(e.Detail, &d); err != nil {
		t.Fatalf("detail not json: %v", err)
	}
	if d["via"] != "pat" || d["method"] != "GET" || d["path"] != "/admin/api/pat/keys" || d["status"] != float64(200) {
		t.Errorf("detail = %v", d)
	}
	// Detail must not contain the query string or any body field.
	for _, k := range []string{"query", "body", "secret"} {
		if _, ok := d[k]; ok {
			t.Errorf("detail must not contain %q: %v", k, d)
		}
	}
}

func TestPATAudit_FailureStatus(t *testing.T) {
	audit := &fakeAuditLogger{}
	patAuditReq(t, audit, int64(1), func(c *gin.Context) { c.Status(http.StatusNotFound) })
	if len(audit.entries) != 1 {
		t.Fatalf("entries = %d", len(audit.entries))
	}
	if audit.entries[0].Status != "failure" {
		t.Errorf("Status = %q, want failure", audit.entries[0].Status)
	}
}

func TestPATAudit_NoPatIDSkipped(t *testing.T) {
	audit := &fakeAuditLogger{}
	patAuditReq(t, audit, nil, func(c *gin.Context) { c.Status(http.StatusOK) })
	if len(audit.entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(audit.entries))
	}
}

func TestPATAudit_NilLoggerSkipped(t *testing.T) {
	w := patAuditReq(t, nil, int64(42), func(c *gin.Context) { c.Status(http.StatusOK) })
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
}

// TestPATAudit_TypedNilPointerDoesNotPanic locks in the production wiring
// hazard: passing a nil *service.AuditService as the interface argument must
// be caught by the guard (reflect-based nil check), not panic on Log. This
// exact bug shipped in the first version — Community edition panicked on
// every PAT request.
func TestPATAudit_TypedNilPointerDoesNotPanic(t *testing.T) {
	var typedNil *service.AuditService // nil concrete pointer
	w := patAuditReq(t, nil, int64(42), func(c *gin.Context) { c.Status(http.StatusOK) })
	_ = typedNil
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	// Direct call with typed nil stuffed into the interface.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(PATAudit(typedNil))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w2.Code != 200 {
		t.Fatalf("typed-nil logger panicked or failed: status = %d", w2.Code)
	}
}
