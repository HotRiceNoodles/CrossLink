package service

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/router"
	"gorm.io/datatypes"
)

// Phase-1 effort eligibility (2026-11-25 checklist §6.1): the candidate filter
// that runs before the fallback engine and before the ceiling reservation.

func effortRoute(adapter, apiVersion string, allowed ...string) *router.RouteResult {
	row := &model.Provider{
		AdapterType: adapter,
		ExtraConfig: datatypes.JSON(`{"api_version":"` + apiVersion + `"}`),
	}
	return &router.RouteResult{
		ProviderRow:       row,
		ProviderModel:     "m-" + adapter,
		AllowedExtensions: allowed,
	}
}

func TestRequestCarriesEffort(t *testing.T) {
	t.Parallel()
	if RequestCarriesEffort(nil) {
		t.Fatal("nil envelope carries nothing")
	}
	env := &domain.ProtocolExtensions{Params: map[string]json.RawMessage{}}
	if RequestCarriesEffort(env) {
		t.Fatal("empty params carries nothing")
	}
	env.Params["anthropic.output_config"] = json.RawMessage(`{"effort":"high"}`)
	if !RequestCarriesEffort(env) {
		t.Fatal("envelope with output_config must carry effort")
	}
}

func TestFilterEffortEligibleRoutes(t *testing.T) {
	t.Parallel()
	routes := []*router.RouteResult{
		effortRoute("anthropic", "2026-10-21"),                                  // not opted in
		effortRoute("anthropic", "2026-10-21", "output_config"),                 // eligible
		effortRoute("anthropic", "2023-06-01", "output_config"),                 // legacy version
		effortRoute("openai_compatible", "v1", "output_config"),                 // eligible (translation)
		effortRoute("azure_openai", "v1", "output_config"),                      // unverified family
		{ProviderModel: "no-row", AllowedExtensions: []string{"output_config"}}, // no provider row
	}
	eligible, skips := FilterEffortEligibleRoutes(routes, true)
	if len(eligible) != 2 {
		t.Fatalf("eligible = %d, want 2", len(eligible))
	}
	if eligible[0].ProviderRow.AdapterType != "anthropic" || eligible[1].ProviderRow.AdapterType != "openai_compatible" {
		t.Fatalf("eligible set wrong: %v", eligible)
	}
	if len(skips) != 4 {
		t.Fatalf("skips = %d, want 4", len(skips))
	}
	wantReasons := []string{
		"route has not opted in to output_config",
		`api_version "2023-06-01" does not accept output_config`,
		"adapter family azure_openai not verified",
		"cannot verify output_config support",
	}
	for i, want := range wantReasons {
		if skips[i].Skipped != true || !errors.Is(skips[i].Error, skips[i].Error) {
			t.Fatalf("skip %d must be marked skipped", i)
		}
		if !containsStr(skips[i].Error.Error(), want) {
			t.Fatalf("skip %d reason = %q, want substring %q", i, skips[i].Error.Error(), want)
		}
	}

	// No-effort requests pass through untouched.
	same, noSkips := FilterEffortEligibleRoutes(routes, false)
	if len(same) != len(routes) || noSkips != nil {
		t.Fatal("no-effort request must pass through unchanged")
	}
}

func TestFilterEffortEligibleRoutes_EmptyIsSentinelTerritory(t *testing.T) {
	t.Parallel()
	eligible, _ := FilterEffortEligibleRoutes([]*router.RouteResult{effortRoute("anthropic", "2026-10-21")}, true)
	if len(eligible) != 0 {
		t.Fatal("sole incompatible candidate must filter to empty (caller maps to the stable 400)")
	}
	// The sentinel is errors.Is-able through RouteError.
	re := &RouteError{Inner: ErrNoEffortCompatibleRoute, Model: "m"}
	if !errors.Is(re, ErrNoEffortCompatibleRoute) {
		t.Fatal("RouteError must unwrap to the sentinel")
	}
}

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
