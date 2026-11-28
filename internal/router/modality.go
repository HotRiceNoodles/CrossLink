package router

// Modality gating (图片方案 P3, design §5.2 评审 F2 — post-resolve filter).
//
// The Resolver is endpoint-agnostic (it resolves model_name → candidates and
// its cache is shared across endpoint families), so modality filtering happens
// POST-resolve at each handler, after fallback expansion — one filter call,
// no Resolver signature change, no cache-key impact.
//
// Semantics ("" = unclassified is the migration default for legacy rows):
//   - modality == ""            → kept by EVERY endpoint family (zero-break:
//     an existing image mapping created before P3 keeps serving /v1/images/*)
//   - modality == want          → kept
//   - modality == other explicit → dropped
//
// AllBlockedByModality reports the distinct drop reason: routes existed but
// every one declared an explicit, incompatible modality — the caller answers
// 400 modality_mismatch (a configuration verdict the admin can act on),
// distinct from "no route at all" (404/resolve error).

// FilterRoutesByModality returns the subset of routes eligible for an
// endpoint family serving `want` (e.g. "text" for chat/messages, "image" for
// images endpoints).
func FilterRoutesByModality(routes []*RouteResult, want string) (kept []*RouteResult, allBlockedByModality bool) {
	for _, r := range routes {
		if r.Modality == "" || r.Modality == want {
			kept = append(kept, r)
		}
	}
	return kept, len(kept) == 0 && len(routes) > 0
}
