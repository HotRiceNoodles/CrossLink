package service

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
)

// Phase-1 effort admission (docs/plans/
// 2026-11-25-extension-param-adaptation-checklist.md §6.1).
//
// Candidate eligibility runs BEFORE the fallback engine and BEFORE budget
// reservation, on the EXPANDED chain. Both placement rules come from the
// review record:
//   - the engine truncates the chain by MaxRetries before any closure runs,
//     so a closure-error "skip" would burn attempt slots and leave the only
//     compatible candidate beyond the truncation line unreachable — and the
//     error would enter the upstream failure classifier, polluting failover;
//   - the ceiling reservation must see the SAME filtered set, so an expensive
//     but incompatible candidate cannot push a servable request into 429.

// ErrNoEffortCompatibleRoute is the stable gateway 400 for a request whose
// output_config.effort no expanded candidate can serve. Handlers map it to
// identical body/error_type/log rows (checklist §6.1 "稳定的网关 400").
var ErrNoEffortCompatibleRoute = errors.New("output_config is not enabled on any route for this model; enable allowed_extensions (output_config) on a route or remove output_config from the request")

// RequestCarriesEffort reports whether the admitted envelope carries
// output_config (Phase-1: the effort subfield, shape-validated at admission).
func RequestCarriesEffort(ext *domain.ProtocolExtensions) bool {
	if ext == nil || len(ext.Params) == 0 {
		return false
	}
	raw, ok := ext.Params["anthropic.output_config"]
	return ok && len(raw) > 0
}

// FilterEffortEligibleRoutes splits the expanded candidate chain into
// effort-eligible routes plus recorded skips for the attempts timeline. A
// request that carries no effort (nil envelope / no output_config) passes
// through unchanged. Source-protocol awareness (reverse mapping,
// 2026-11-26): an openai-NATIVE carry (reasoning_effort envelope-ized at
// admission) does not gate openai-protocol routes — the param is native
// there and pre-existing traffic must not regress; anthropic routes still
// require opt-in + version caps (cross-protocol synthesis is the gated act).
func FilterEffortEligibleRoutes(routes []*router.RouteResult, ext *domain.ProtocolExtensions) ([]*router.RouteResult, []FallbackAttempt) {
	if !RequestCarriesEffort(ext) {
		return routes, nil
	}
	openaiNative := ext != nil && ext.SourceProtocol == "openai"
	eligible := make([]*router.RouteResult, 0, len(routes))
	var skips []FallbackAttempt
	for _, r := range routes {
		if reason := effortSkipReason(r, openaiNative); reason != "" {
			skips = append(skips, FallbackAttempt{
				ProviderName:  routeProviderName(r),
				ProviderModel: r.ProviderModel,
				ErrorType:     provider.ErrorType("incompatible_route"),
				Error:         errors.New(reason),
				Skipped:       true,
			})
			continue
		}
		eligible = append(eligible, r)
	}
	return eligible, skips
}

func routeProviderName(r *router.RouteResult) string {
	if r.Provider != nil {
		return r.Provider.Name()
	}
	return ""
}

// effortSkipReason returns the human-readable incompatibility of a candidate
// that cannot serve an effort-carrying request; "" when eligible.
//
// Eligibility = route opt-in (allowed_extensions, the admin's verification of
// the actual upstream combination — checklist "首发范围收敛") AND adapter
// family capability (anthropic: registered version caps; openai_compatible:
// reasoning_effort translation; everything else conservatively incompatible).
func effortSkipReason(r *router.RouteResult, openaiNative bool) string {
	row := r.ProviderRow
	if row != nil && openaiNative && row.AdapterType == "openai_compatible" {
		// Reverse mapping: openai-native effort on its native protocol — no
		// cross-protocol act, no gate (pre-existing traffic).
		return ""
	}
	if !routeAllowsOutputConfig(r) {
		return "route has not opted in to output_config (allowed_extensions)"
	}
	if row == nil {
		return "candidate carries no provider row: cannot verify output_config support"
	}
	switch row.AdapterType {
	case "anthropic":
		ver := providerAPIVersion(row)
		if !provider.AnthropicVersionSupportsEffort(ver) {
			return fmt.Sprintf("api_version %q does not accept output_config", ver)
		}
		return ""
	case "openai_compatible":
		// effort translates to reasoning_effort at serialization (registered
		// openai wire param); model-level acceptance is the route opt-in's
		// verification duty.
		return ""
	default:
		return "adapter family " + row.AdapterType + " not verified for output_config"
	}
}

func routeAllowsOutputConfig(r *router.RouteResult) bool {
	for _, w := range r.AllowedExtensions {
		if w == "output_config" {
			return true
		}
	}
	return false
}

func providerAPIVersion(row *model.Provider) string {
	var cfg struct {
		APIVersion string `json:"api_version"`
	}
	_ = json.Unmarshal(row.ExtraConfig, &cfg)
	return cfg.APIVersion
}
