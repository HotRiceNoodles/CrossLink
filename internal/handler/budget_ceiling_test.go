package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crosslink/internal/router"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// M5: the ceiling reservation — estimate covers the most expensive candidate
// of the expanded chain, so a cheap-primary → pricey-fallback failover cannot
// spend past what was reserved.

func TestBudgetCeilingEstimate(t *testing.T) {
	t.Parallel()
	cheap := &router.RouteResult{InputPrice: 0.001, OutputPrice: 0.002}
	pricey := &router.RouteResult{InputPrice: 0.01, OutputPrice: 0.02}
	unmetered := &router.RouteResult{}

	// The ceiling must be the pricey candidate's estimate, not the primary's.
	ceiling := budgetCeilingEstimate(1000, 500, []*router.RouteResult{cheap, pricey, nil})
	want := (float64(1000)*0.01 + float64(500)*0.02) / 1000
	assert.Equal(t, want, ceiling)

	// All candidates unmetered → zero → legacy skip-reservation behavior.
	assert.Zero(t, budgetCeilingEstimate(1000, 500, []*router.RouteResult{unmetered}))
	assert.Zero(t, budgetCeilingEstimate(1000, 500, nil))
}

func TestReserveBudgetCeilingSkipPaths(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	routes := []*router.RouteResult{{InputPrice: 0.01, OutputPrice: 0.02}}

	// No budget service (feature off) → proceed.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	assert.True(t, reserveBudgetCeilingForRequest(c, nil, nil, 100, 500, routes, 0))

	// No routes → proceed (caller guards len(routes) > 0; belt-and-braces).
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2.Request, _ = http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	assert.True(t, reserveBudgetCeilingForRequest(c2, nil, nil, 100, 500, nil, 0))
}
