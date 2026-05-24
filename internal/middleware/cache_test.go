package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/crosslink/internal/crypto"
)

func mockContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func testCP() crypto.CryptoProvider {
	p, _ := crypto.NewProvider("standard")
	return p
}

func TestBuildCacheKey_ExcludesStreamAndUser(t *testing.T) {
	body1, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
		"stream":   false,
		"user":     "alice",
	})
	body2, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
		"stream":   true,
		"user":     "bob",
	})

	c := mockContext()
	key1 := buildCacheKey("/v1/chat/completions", body1, c, testCP())
	key2 := buildCacheKey("/v1/chat/completions", body2, c, testCP())

	if key1 != key2 {
		t.Errorf("expected same key after excluding stream/user, got\n%s\n%s", key1, key2)
	}
}

func TestBuildCacheKey_DifferentPaths(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
	})

	c := mockContext()
	key1 := buildCacheKey("/v1/chat/completions", body, c, testCP())
	key2 := buildCacheKey("/v1/messages", body, c, testCP())

	if key1 == key2 {
		t.Error("expected different keys for different paths")
	}
}

func TestBuildCacheKey_DifferentContent(t *testing.T) {
	body1, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
	})
	body2, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"goodbye"},
	})

	c := mockContext()
	key1 := buildCacheKey("/v1/chat/completions", body1, c, testCP())
	key2 := buildCacheKey("/v1/chat/completions", body2, c, testCP())

	if key1 == key2 {
		t.Error("expected different keys for different content")
	}
}

func TestBuildCacheKey_InvalidJSON(t *testing.T) {
	c := mockContext()
	key := buildCacheKey("/v1/chat/completions", []byte("not json"), c, testCP())
	if key == "" {
		t.Error("expected non-empty key for invalid JSON")
	}
}

func TestBuildCacheKey_ExcludesStreamOptions(t *testing.T) {
	body1, _ := json.Marshal(map[string]interface{}{
		"model":          "gpt-4",
		"messages":       []string{"hello"},
		"stream_options": map[string]bool{"include_usage": true},
	})
	body2, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
	})

	c := mockContext()
	key1 := buildCacheKey("/v1/chat/completions", body1, c, testCP())
	key2 := buildCacheKey("/v1/chat/completions", body2, c, testCP())

	if key1 != key2 {
		t.Error("expected same key after excluding stream_options")
	}
}

func TestBuildCacheKey_DifferentUsers(t *testing.T) {
	body, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": []string{"hello"},
	})

	c1 := mockContext()
	c1.Set("api_key_id", int64(1))

	c2 := mockContext()
	c2.Set("api_key_id", int64(2))

	key1 := buildCacheKey("/v1/chat/completions", body, c1, testCP())
	key2 := buildCacheKey("/v1/chat/completions", body, c2, testCP())

	if key1 == key2 {
		t.Error("expected different keys for different user identities")
	}
}
