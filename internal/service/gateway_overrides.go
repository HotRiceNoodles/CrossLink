package service

import (
	"encoding/json"

	"github.com/crosslink/internal/admission"
	"github.com/crosslink/internal/debug/upstream"
	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/router"
)

// ApplyRouteRequestOverrides is the EXECUTION half of the M3.2 model-level
// extension controls (the write half is router.ValidateModelExtensionConfig
// at config time — double validation per the plan). It runs inside the
// per-candidate closure, on the CLONED request + envelope, so two candidates
// never observe each other's effective params.
//
// Priority order (plan M3.2): 模型默认值 < 客户端允许参数 < 管理员允许覆盖 —
// an admin override WINS over a client-sent value.
//
// Dialect gating: a client-carried dialect param (envelope Params) is only
// forwarded when the route opted in via allowed_extensions; an admin
// request_overrides entry implies opt-in for that param.
func ApplyRouteRequestOverrides(req *domain.OpenAIRequest, route *router.RouteResult) {
	if route == nil {
		return
	}
	if len(route.RequestOverrides) > 0 {
		// Registered generation params only (validated at write time; the
		// execution half re-checks the key set defensively — a stale row
		// written before validation existed must not smuggle fields through).
		if v, ok := route.RequestOverrides["temperature"].(float64); ok && router.ModelOverrideFields["temperature"] != "" {
			req.Temperature = &v
		}
		if v, ok := route.RequestOverrides["top_p"].(float64); ok && router.ModelOverrideFields["top_p"] != "" {
			req.TopP = &v
		}
		if v, ok := route.RequestOverrides["enable_thinking"].(bool); ok && router.ModelOverrideFields["enable_thinking"] != "" {
			setDialectParam(req, "dialect.enable_thinking", v)
		}
	}
	// Gate client-carried dialect params: strip everything the route has not
	// opted in to (admin overrides re-add theirs above).
	if req.Extensions != nil && len(req.Extensions.Params) > 0 {
		allowed := routeExtensionSet(route)
		for key := range req.Extensions.Params {
			wire, isDialect := admission.DialectWireName(key)
			if !isDialect {
				continue
			}
			if !allowed[wire] {
				delete(req.Extensions.Params, key)
			}
		}
	}
}

func routeExtensionSet(route *router.RouteResult) map[string]bool {
	allowed := make(map[string]bool, len(route.AllowedExtensions)+len(route.RequestOverrides))
	for _, w := range route.AllowedExtensions {
		allowed[w] = true
	}
	for w := range route.RequestOverrides {
		allowed[w] = true
	}
	return allowed
}

func setDialectParam(req *domain.OpenAIRequest, envelopeKey string, value any) {
	if req.Extensions == nil {
		req.Extensions = &domain.ProtocolExtensions{SourceProtocol: "openai"}
	}
	if req.Extensions.Params == nil {
		req.Extensions.Params = make(map[string]json.RawMessage)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	req.Extensions.Params[envelopeKey] = raw
}

// attachStripsToAttempts zips the per-closure strip batches onto the engine's
// attempt records: callFn (which wraps every non-skipped attempt) runs
// synchronously in engine order, so batch i belongs to the i-th non-skipped
// attempt. Provider-name checked defensively; a mismatch (engine internals
// changing) drops the batch rather than mis-attributing audit records.
// AttachStripsToAttempts is the exported form used by both the GatewayService
// closures and the openai handler's own engine closures (identical pattern).
func AttachStripsToAttempts(attempts []FallbackAttempt, batches [][]upstream.ParamStrip) {
	if len(batches) == 0 {
		return
	}
	i := 0
	for k := range attempts {
		if attempts[k].Skipped {
			continue
		}
		if i >= len(batches) {
			return
		}
		if len(batches[i]) > 0 && attempts[k].ProviderName == batches[i][0].Provider {
			attempts[k].ParamStrips = batches[i]
		}
		i++
	}
}
