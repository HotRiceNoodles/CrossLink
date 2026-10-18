package handler

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/router"
	"github.com/gin-gonic/gin"
)

// Ladder cap2/cap3 finding (F-4): gateway-side temporary rejections (503) had
// no backoff hint — a fail-fast 503 makes closed-loop clients instantly
// re-submit, which during a port-exhaustion outage self-sustained into an
// 87k-attempt dial storm. The 503 must carry Retry-After.
func TestResolveRetryAfterHeader(t *testing.T) {
	degraded := fmt.Errorf("%w: query model mappings: dial refused", router.ErrResolveDegraded)
	noActive := fmt.Errorf("%w: all routes circuit-open", router.ErrNoActiveProvider)

	for name, err := range map[string]error{
		"resolve-degraded": degraded,
		"no-active":        noActive,
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			resolveRetryAfterHeader(c, err)
			if got := w.Header().Get("Retry-After"); got == "" {
				t.Fatalf("temporary gateway rejection must set Retry-After (err=%v)", err)
			}
		})
	}

	t.Run("permanent-errors-untouched", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		resolveRetryAfterHeader(c, errors.New("no provider found for model: x"))
		if got := w.Header().Get("Retry-After"); got != "" {
			t.Fatalf("non-temporary error must not set Retry-After, got %q", got)
		}
	})
}
