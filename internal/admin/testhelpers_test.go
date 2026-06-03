package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestContext creates a gin.Context backed by an httptest.NewRecorder.
// The body is serialized as JSON when non-nil.
func newTestContext(t *testing.T, method, path string, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, path, bodyReader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

// setAdminContext simulates the JWT middleware by setting auth context values.
func setAdminContext(c *gin.Context, userID int64, roleID int64, roleName string) {
	c.Set("user_id", userID)
	c.Set("username", "test-user")
	c.Set("role_id", roleID)
	c.Set("role_name", roleName)
	c.Set("team_id", int64(0))
}

// setPathParams sets Gin path parameters on the context.
func setPathParams(c *gin.Context, params gin.Params) {
	c.Params = params
}

// decodeResponse parses the JSON response body into target.
func decodeResponse(t *testing.T, w *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(w.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, w.Body.String())
	}
}

// testInt64Ptr returns a pointer to the given int64 value.
// Named differently from int64Ptr in commercial overlay's organizations.go
// to avoid redeclaration when the overlay is merged.
func testInt64Ptr(v int64) *int64 { return &v }
