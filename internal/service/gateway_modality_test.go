package service

import (
	"errors"
	"testing"

	"github.com/crosslink/internal/router"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// P3: the modality gate wraps FilterRoutesByModality with the stable 400
// sentinel — same shape as the effort gate (gateway_effort_test.go).

func TestGateRoutesForText(t *testing.T) {
	t.Parallel()
	t.Run("unclassified passes untouched", func(t *testing.T) {
		t.Parallel()
		routes := []*router.RouteResult{{Modality: ""}, {Modality: "text"}}
		kept, err := GateRoutesForText(routes)
		require.NoError(t, err)
		assert.Len(t, kept, 2)
	})
	t.Run("image-classified rejected with sentinel", func(t *testing.T) {
		t.Parallel()
		_, err := GateRoutesForText([]*router.RouteResult{{Modality: "image"}, {Modality: "video"}})
		require.ErrorIs(t, err, ErrModalityMismatch)
		// RouteError wrapping must preserve errors.Is for the handler's 400
		// mapping (same contract as ErrNoEffortCompatibleRoute).
		re := &RouteError{Inner: err, Model: "m"}
		assert.True(t, errors.Is(re, ErrModalityMismatch))
	})
	t.Run("mixed keeps text and unclassified", func(t *testing.T) {
		t.Parallel()
		kept, err := GateRoutesForText([]*router.RouteResult{
			{Modality: ""}, {Modality: "text"}, {Modality: "image"},
		})
		require.NoError(t, err)
		assert.Len(t, kept, 2, "image-classified route is dropped from a text endpoint")
	})
}
