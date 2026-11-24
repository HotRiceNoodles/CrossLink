package service

import (
	"time"

	"github.com/crosslink/internal/model"
)

// KeyWindowPolicy decides whether a validated API key may be used at the
// given instant (time-window control, docs/design/api-key-time-window-control.md).
// Community ships NoopWindowPolicy (always allow); the commercial overlay
// injects the parser/evaluator implementation, Pro-gated.
//
// Mirrors the IPPolicy seam split: the interface lives in service so the
// overlay implementation (parsing + license gate) stays out of middleware,
// and middleware never grows edition-specific code.
type KeyWindowPolicy interface {
	// Check returns nil if now falls inside the key's allowed time windows
	// (or the key carries none — NULL/empty column is unrestricted legacy
	// data). A non-nil *TimeWindowError denies the request (mapped to 403).
	Check(key *model.APIKey, now time.Time) error
}

// NoopWindowPolicy is the Community default: always allows. The
// AllowedTimeWindows field, if populated, is ignored in Community builds.
type NoopWindowPolicy struct{}

func (NoopWindowPolicy) Check(*model.APIKey, time.Time) error { return nil }

// TimeWindowError is the denial verdict carried across the seam. Invalid
// distinguishes a fail-closed verdict (stored policy present but unparseable
// → time_window_invalid) from a plain outside-window denial
// (time_window_denied); NextOpen is the next opening instant (UTC) when
// computable, used for the response message and the Retry-After header; TZ is
// the policy's IANA zone label ("" = UTC) so the denial message can name the
// opening instant in the key holder's own wall clock.
type TimeWindowError struct {
	Invalid  bool
	NextOpen time.Time
	TZ       string
}

func (e *TimeWindowError) Error() string {
	if e.Invalid {
		return "time window policy invalid"
	}
	if e.NextOpen.IsZero() {
		return "outside allowed time windows"
	}
	return "outside allowed time windows until " + e.NextOpen.Format(time.RFC3339)
}
