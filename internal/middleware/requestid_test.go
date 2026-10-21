package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func run(t *testing.T, header string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if header != "" {
		c.Request.Header.Set("X-Request-ID", header)
	}
	RequestID()(c)
	return c, w
}

// The INTERNAL billing id is always a full server-minted UUID v4 — never
// the 8-hex mint (birthday-collidable: 41 collisions in r59's 607,854) and
// never client-influenced.
func TestRequestID_InternalAlwaysFullUUID(t *testing.T) {
	for i := 0; i < 1000; i++ {
		c, _ := run(t, "")
		id := c.GetString("request_id")
		if !uuidV4.MatchString(id) {
			t.Fatalf("internal id %q is not a full UUID v4", id)
		}
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("internal id %q unparseable: %v", id, err)
		}
	}
}

// Uniqueness at scale: no collisions in 100k mints (the 8-hex mint
// collides ~1/93k; 122-bit ids make this probabilistically impossible).
func TestRequestID_UniqueAtScale(t *testing.T) {
	seen := make(map[string]struct{}, 100000)
	for i := 0; i < 100000; i++ {
		id := uuid.New().String()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision on %q at %d", id, i)
		}
		seen[id] = struct{}{}
	}
}

// A client replaying ONE X-Request-ID across requests must NOT share the
// billing id: each request gets a distinct internal id (the old middleware
// used the header verbatim — a systematic Redis-billing bypass). The
// client value survives only as client_request_id (tracing), and is echoed
// in the response for correlation.
func TestRequestID_ClientHeaderSeparatedFromBillingID(t *testing.T) {
	const evil = "reused-client-id"
	c1, w1 := run(t, evil)
	c2, w2 := run(t, evil)
	i1, i2 := c1.GetString("request_id"), c2.GetString("request_id")
	if i1 == i2 {
		t.Fatal("same client X-Request-ID produced the SAME internal billing id — billing bypass class")
	}
	if !uuidV4.MatchString(i1) || !uuidV4.MatchString(i2) {
		t.Fatalf("internal ids not full UUIDs: %q %q", i1, i2)
	}
	for _, c := range []*gin.Context{c1, c2} {
		if got := c.GetString("client_request_id"); got != evil {
			t.Fatalf("client_request_id = %q, want %q (tracing preserved)", got, evil)
		}
	}
	if w1.Header().Get("X-Request-ID") != evil || w2.Header().Get("X-Request-ID") != evil {
		t.Fatal("response must echo the client's id for correlation")
	}
}

func TestRequestID_NoHeader_InternalEchoed(t *testing.T) {
	c, w := run(t, "")
	if w.Header().Get("X-Request-ID") != c.GetString("request_id") {
		t.Fatal("without a client id the response must echo the internal id")
	}
	if _, exists := c.Get("client_request_id"); exists {
		t.Fatal("client_request_id must not be set when no header supplied")
	}
}

// Invalid/oversized client headers are dropped from tracing entirely; the
// billing id is unaffected (still a fresh UUID).
func TestRequestID_InvalidClientHeaderDropped(t *testing.T) {
	for _, bad := range []string{"abc!def", "with space", "tab\tid", string(make([]byte, 0))} {
		c, _ := run(t, bad)
		if !uuidV4.MatchString(c.GetString("request_id")) {
			t.Fatalf("billing id affected by invalid header %q", bad)
		}
		if _, exists := c.Get("client_request_id"); bad != "" && exists {
			t.Fatalf("invalid header %q must not be kept for tracing", bad)
		}
	}
	c, _ := run(t, "a-very-long-client-id-that-exceeds-the-sixty-four-character-sanitization-limit-xxxxxxxxxxxxxxxx")
	if got := c.GetString("client_request_id"); len(got) != 64 {
		t.Fatalf("oversized header not truncated to 64: len=%d", len(got))
	}
}
