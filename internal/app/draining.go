package app

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// DrainingManager tracks in-flight gateway requests and supports graceful drain
// during server shutdown. It should be applied ONLY to the gateway route group,
// not admin or health endpoints.
type DrainingManager struct {
	wg       sync.WaitGroup
	draining atomic.Bool
	active   atomic.Int64
}

func NewDrainingManager() *DrainingManager {
	return &DrainingManager{}
}

func (d *DrainingManager) IsDraining() bool {
	return d.draining.Load()
}

func (d *DrainingManager) ActiveCount() int64 {
	return d.active.Load()
}

// Middleware tracks in-flight requests for graceful draining.
// During drain, new requests receive 503 immediately.
func (d *DrainingManager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.IsDraining() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			c.Abort()
			return
		}
		d.wg.Add(1)
		d.active.Add(1)
		defer func() {
			d.wg.Done()
			d.active.Add(-1)
		}()
		c.Next()
	}
}

// Drain sets the draining flag and waits for in-flight requests to complete
// up to the given timeout. Returns the number of requests still active if
// timeout was exceeded.
func (d *DrainingManager) Drain(timeout time.Duration) int64 {
	d.draining.Store(true)

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return 0
	case <-time.After(timeout):
		return d.active.Load()
	}
}
