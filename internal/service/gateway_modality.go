package service

import (
	"errors"

	"github.com/crosslink/internal/router"
)

// ErrModalityMismatch is the stable gateway 400 for a request whose model
// routes exist but every one declares an explicit modality incompatible with
// the endpoint family (图片方案 P3, design §5.2) — e.g. a modality="image"
// mapping sent to /v1/chat/completions. A configuration verdict the admin can
// act on, deliberately distinct from "no route" (404/resolve error).
var ErrModalityMismatch = errors.New("model is not configured for this endpoint family (modality_mismatch); check the mapping's modality")

// gateRoutesForModality applies the P3 post-resolve modality gate for the
// endpoint family serving `want` ("text" for chat/messages). Kept: modality ""
// (unclassified legacy — zero-break) or an exact match. Returns the kept
// chain, or ErrModalityMismatch when routes exist but ALL are explicitly
// incompatible.
func gateRoutesForModality(routes []*router.RouteResult, want string) ([]*router.RouteResult, error) {
	kept, allBlocked := router.FilterRoutesByModality(routes, want)
	if allBlocked {
		return nil, ErrModalityMismatch
	}
	return kept, nil
}

// GateRoutesForText is the handler-facing wrapper for chat/messages endpoint
// families (the openai handler owns its route chain and gates in-process;
// GatewayService re-applies the gate on its own resolve — double-gate, same
// pattern as effort eligibility).
func GateRoutesForText(routes []*router.RouteResult) ([]*router.RouteResult, error) {
	return gateRoutesForModality(routes, "text")
}
