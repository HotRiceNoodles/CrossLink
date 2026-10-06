package apidoc

import "time"

// Deprecation describes one deprecated API route. This registry is the single
// source of truth for deprecation state — it is Go code on purpose, so every
// deprecation shows up in review diffs and compiles into the binary that
// enforces it. The OpenAPI bundle mirrors this via x-sunset extensions for
// documentation only.
type Deprecation struct {
	// Route is the "METHOD /path" key (path without :id params resolved, e.g.
	// "GET /portal/api/usage").
	Route string `json:"route"`

	// DeprecatedAt is when the deprecation was announced (release date).
	DeprecatedAt time.Time `json:"deprecated_at"`

	// SunsetAt is when the route stops working. Policy (docs/api-versioning.md):
	// at least 6 months after announcement, 12 months during 0.x.
	SunsetAt time.Time `json:"sunset_at"`

	// Successor is the replacement route, if any.
	Successor string `json:"successor,omitempty"`

	// Reason is a short human-readable explanation.
	Reason string `json:"reason"`
}

// ActiveDeprecations lists routes whose deprecation has been announced but
// which are still served. Keep entries here for the full sunset period, then
// remove the route AND its entry in the same release.
//
// Lifecycle: register here -> Deprecation/Sunset headers served -> remove at
// SunsetAt (see docs/api-versioning.md).
var ActiveDeprecations = []Deprecation{}
