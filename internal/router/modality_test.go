package router

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// P3 (design §5.2): post-resolve modality filtering. "" is the migration
// default for legacy rows and must be kept by EVERY family (zero-break);
// explicit values gate.

func modRoute(modality string) *RouteResult {
	return &RouteResult{Modality: modality}
}

func TestFilterRoutesByModality(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		modalities  []string
		want        string
		wantKept    int
		wantBlocked bool
	}{
		{"empty routes", nil, "text", 0, false},
		{"all unclassified kept for any family", []string{"", "", ""}, "text", 3, false},
		{"unclassified kept for image family too", []string{""}, "image", 1, false},
		{"exact match kept", []string{"text", "image", "text"}, "text", 2, false},
		{"mixed unclassified + match", []string{"", "image"}, "image", 2, false},
		{"all explicitly incompatible", []string{"image", "image"}, "text", 0, true},
		{"single incompatible", []string{"embedding"}, "text", 0, true},
		{"video vs image", []string{"video"}, "image", 0, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			routes := make([]*RouteResult, len(tc.modalities))
			for i, m := range tc.modalities {
				routes[i] = modRoute(m)
			}
			kept, allBlocked := FilterRoutesByModality(routes, tc.want)
			assert.Len(t, kept, tc.wantKept)
			assert.Equal(t, tc.wantBlocked, allBlocked, "allBlockedByModality flag")
			if !tc.wantBlocked {
				for _, r := range kept {
					assert.True(t, r.Modality == "" || r.Modality == tc.want, "kept route must be unclassified or match")
				}
			}
		})
	}
}
