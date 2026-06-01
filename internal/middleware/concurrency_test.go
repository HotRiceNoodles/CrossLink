package middleware

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConcurrencyLimit_UnderLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(ConcurrencyLimit(2))
	r.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestConcurrencyLimit_AtLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := ConcurrencyLimit(1)

	block := make(chan struct{})
	started := make(chan struct{})

	r := gin.New()
	r.Use(handler)
	r.GET("/", func(c *gin.Context) {
		close(started)
		<-block
	})

	go func() {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(w, req)
	}()

	<-started // wait for first request to hold the slot

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	close(block)
}

func TestConcurrencyLimit_Releases(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var wg sync.WaitGroup

	r := gin.New()
	r.Use(ConcurrencyLimit(1))
	r.GET("/", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	// First request completes
	wg.Add(1)
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(w1, req1)
	wg.Done()

	// Second request should also succeed
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w2.Code, http.StatusOK)
	}
}
