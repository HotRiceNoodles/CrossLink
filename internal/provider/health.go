package provider

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

type circuitKind int8

const (
	kindTransient circuitKind = iota
	kindPersistent
)

// CircuitState is a snapshot of a circuit breaker's state (pure read, does not
// affect probeInFlight single-flight semantics). Higher value = more severe.
type CircuitState int8

const (
	CircuitClosed   CircuitState = iota // healthy (no failures or cleared)
	CircuitHalfOpen                     // openUntil expired, awaiting probe outcome
	CircuitOpen                         // blocking (time.Now().Before(openUntil))
)

// String returns the lowercase state name used by the health snapshot API.
func (s CircuitState) String() string {
	switch s {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// ProviderHealthSnapshot is a copy of one circuit's state — never a reference
// to internal tracker state.
type ProviderHealthSnapshot struct {
	Provider string
	Model    string
	State    string    // "closed" / "half_open" / "open"
	Until    time.Time // cooldown deadline; zero value = none
	// OpenCount is how many times this circuit has opened in a row without an
	// intervening recovery (escalating cooldown factor).
	OpenCount int
	// LastErrorType/LastErrorMessage are the classified error type and upstream
	// message that opened the circuit (set via RecordCause by the fallback engine).
	LastErrorType    string
	LastErrorMessage string
}

// Snapshot returns a copy of every tracked circuit keyed by provider (account
// scope) or provider|model (model scope). Safe for concurrent use.
func (h *HealthTracker) Snapshot() []ProviderHealthSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]ProviderHealthSnapshot, 0, len(h.states))
	for k, s := range h.states {
		name, model := k, ""
		if i := strings.LastIndex(k, "|"); i >= 0 {
			name, model = k[:i], k[i+1:]
		}
		snap := ProviderHealthSnapshot{
			Provider:         name,
			Model:            model,
			OpenCount:        s.openCount,
			LastErrorType:    s.lastErrType,
			LastErrorMessage: s.lastErrMsg,
		}
		if s.openUntil.IsZero() {
			snap.State = CircuitClosed.String()
		} else {
			snap.Until = s.openUntil
			if now.Before(s.openUntil) {
				snap.State = CircuitOpen.String()
			} else {
				snap.State = CircuitHalfOpen.String()
			}
		}
		out = append(out, snap)
	}
	return out
}

type circuitState struct {
	kind             circuitKind
	consecutiveFails int
	openUntil        time.Time
	probeInFlight    bool // half-open single-flight gate (C2)
	openCount        int  // consecutive opens without recovery — cooldown escalation factor
	lastErrType      string
	lastErrMsg       string // truncated upstream message (RecordCause)
}

// HealthTracker tracks per-key circuit state. Keys: account scope → provider name;
// model scope → "name|model". It is process-local (per-instance); multi-replica
// deployments do not share circuit state (see design §4.2⑤).
type HealthTracker struct {
	mu                 sync.Mutex
	states             map[string]*circuitState
	transientThreshold int
	persistentCooldown time.Duration
	transientCooldown  time.Duration
	maxEscalation      time.Duration // cap for the escalating transient cooldown
	retryAfterMin      time.Duration
	retryAfterMax      time.Duration
}

func NewHealthTracker() *HealthTracker {
	return NewHealthTrackerWithConfig(3, 60*time.Second)
}

func NewHealthTrackerWithConfig(failThreshold int, openDuration time.Duration) *HealthTracker {
	if failThreshold <= 0 {
		failThreshold = 3
	}
	if openDuration <= 0 {
		openDuration = 60 * time.Second
	}
	return &HealthTracker{
		states:             make(map[string]*circuitState),
		transientThreshold: failThreshold,
		persistentCooldown: 30 * time.Minute,
		transientCooldown:  openDuration,
		maxEscalation:      5 * time.Minute,
		retryAfterMin:      5 * time.Second,
		retryAfterMax:      5 * time.Minute,
	}
}

// key returns the circuit key for a (name, model, scope) tuple.
func (h *HealthTracker) key(name, model, scope string) string {
	if scope == "model" && model != "" {
		return name + "|" + model
	}
	return name
}

// IsHealthy reports whether the account-level circuit for name is healthy.
// Backward-compatible shim; model-aware callers should use IsHealthyModel.
func (h *HealthTracker) IsHealthy(name string) bool {
	return h.IsHealthyModel(name, "")
}

// IsHealthyModel reports whether (name, model) is healthy, checking both the account
// key and the model key. When an expired (half-open) circuit is found, exactly one
// caller is allowed to probe (probeInFlight); concurrent callers are told unhealthy so
// they fall back instead of stampeding the just-recovered upstream (C2).
func (h *HealthTracker) IsHealthyModel(name, model string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := []string{name}
	if model != "" {
		keys = append(keys, name+"|"+model)
	}
	// Pass 1: any hard block (still open, or expired but another caller is probing)?
	for _, k := range keys {
		if h.blockedOrProbed(k) {
			return false
		}
	}
	// Pass 2: become the probe for any expired key (set probeInFlight).
	for _, k := range keys {
		if s := h.states[k]; s != nil && !s.openUntil.IsZero() && time.Now().After(s.openUntil) {
			s.probeInFlight = true
		}
	}
	return true
}

// blockedOrProbed reports whether key k blocks this caller: still-open, or expired but
// already being probed by another caller.
func (h *HealthTracker) blockedOrProbed(k string) bool {
	s := h.states[k]
	if s == nil || s.openUntil.IsZero() {
		return false
	}
	if time.Now().Before(s.openUntil) {
		return true // still open
	}
	return s.probeInFlight // expired half-open: blocked iff someone else is probing
}

// CircuitState returns the worst circuit state across the account and model keys
// for (name, model). PURE READ — it does NOT touch probeInFlight, so callers can
// observe open/half-open without disturbing the single-flight probe gating that
// IsHealthyModel relies on (design §4.3 / P4-3).
func (h *HealthTracker) CircuitState(name, model string) CircuitState {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := []string{name}
	if model != "" {
		keys = append(keys, name+"|"+model)
	}
	worst := CircuitClosed
	now := time.Now()
	for _, k := range keys {
		s := h.states[k]
		if s == nil || s.openUntil.IsZero() {
			continue
		}
		var st CircuitState
		if now.Before(s.openUntil) {
			st = CircuitOpen
		} else {
			st = CircuitHalfOpen
		}
		if st > worst {
			worst = st
		}
	}
	return worst
}

// RecordSuccess clears the account circuit (backward-compat shim, account-only).
func (h *HealthTracker) RecordSuccess(name string) { h.RecordSuccessModel(name, "") }

// RecordSuccessModel clears the model key unconditionally and the account key only if
// it is transient or half-open. A non-expired persistent account circuit is NEVER
// cleared by a success — this closes the race where a call that passed IsHealthy just
// before another call set a persistent circuit would otherwise wipe that circuit (C1).
func (h *HealthTracker) RecordSuccessModel(name, model string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if model != "" {
		delete(h.states, name+"|"+model)
	}
	if s := h.states[name]; s != nil {
		if s.kind == kindPersistent && time.Now().Before(s.openUntil) {
			return // persistent circuit still active — leave it
		}
		delete(h.states, name)
	}
}

// RecordFailure is a backward-compat shim equivalent to a transient failure with no
// Retry-After hint. Kept so existing callers/tests compile (B7).
func (h *HealthTracker) RecordFailure(name string) { h.RecordTransientFailure(name, "", 0) }

// RecordTransientFailure records a transient failure on the account key. After
// transientThreshold consecutive failures the circuit opens for the transient cooldown
// (clamped Retry-After if provided). A half-open probe failure resets the count.
func (h *HealthTracker) RecordTransientFailure(name, model string, retryAfter time.Duration) {
	_ = model // transient failures are account-scoped
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.states[name]
	if s == nil {
		s = &circuitState{kind: kindTransient}
		h.states[name] = s
	}
	now := time.Now()
	if !s.openUntil.IsZero() && now.After(s.openUntil) {
		// half-open probe failed: re-arm from a fresh count, close but watching.
		s.consecutiveFails = 1
		s.openUntil = time.Time{}
		s.probeInFlight = false
		return
	}
	s.consecutiveFails++
	s.probeInFlight = false
	if s.consecutiveFails >= h.transientThreshold {
		s.kind = kindTransient
		s.openCount++
		var open time.Duration
		if retryAfter > 0 {
			// Upstream sent a Retry-After hint: it knows best, respect it
			// (clamped) without escalating.
			open = h.clampRetryAfter(retryAfter)
		} else {
			open = h.escalatedCooldown(s.openCount)
		}
		s.openUntil = now.Add(open)
	}
}

// escalatedCooldown returns the transient cooldown for the n-th consecutive
// open: base × 2^(n-1), capped at maxEscalation. A flapping upstream is
// re-probed progressively less often; the base stays short for fast recovery.
func (h *HealthTracker) escalatedCooldown(openCount int) time.Duration {
	shift := openCount - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 16 {
		shift = 16 // overflow guard; maxEscalation bounds the rest
	}
	d := h.transientCooldown << uint(shift)
	if d <= 0 || d > h.maxEscalation {
		return h.maxEscalation
	}
	return d
}

// RecordPersistentFailure opens the circuit for (name, model, scope) immediately and
// for the full persistent cooldown — persistent failures (quota/billing) are
// definitive and do not wait for a threshold ("one strike").
func (h *HealthTracker) RecordPersistentFailure(name, model, scope string, cooldown time.Duration) {
	if cooldown <= 0 {
		cooldown = h.persistentCooldown
	}
	k := h.key(name, model, scope)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.states[k]
	if s == nil {
		s = &circuitState{}
		h.states[k] = s
	}
	s.kind = kindPersistent
	s.openUntil = time.Now().Add(cooldown)
	s.consecutiveFails = 0
	s.probeInFlight = false
}

// ClearProbe releases the probe lease on both keys. Must be called unconditionally by
// the FallbackEngine after an attempt (success, failure, or context cancellation) to
// guarantee probeInFlight never gets stuck (C2 hardening).
func (h *HealthTracker) ClearProbe(name, model string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.states[name]; s != nil {
		s.probeInFlight = false
	}
	if model != "" {
		if s := h.states[name+"|"+model]; s != nil {
			s.probeInFlight = false
		}
	}
}

func (h *HealthTracker) clampRetryAfter(d time.Duration) time.Duration {
	if d <= 0 {
		return h.transientCooldown
	}
	if d < h.retryAfterMin {
		return h.retryAfterMin
	}
	if d > h.retryAfterMax {
		return h.retryAfterMax
	}
	return d
}

// ResolveCircuit unconditionally clears both keys for (name, model). It is
// called ONLY by the out-of-band CircuitProber, whose dedicated probe is an
// authoritative recovery signal — unlike real traffic, which keeps the C1
// guard (a racing success must not clear a live persistent circuit). This is
// what lets quota/persistent circuits close as soon as the upstream is
// verified healthy again instead of waiting out the full cooldown.
func (h *HealthTracker) ResolveCircuit(name, model string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, name)
	if model != "" {
		delete(h.states, name+"|"+model)
	}
}

// RecordCause stores the classified error type and upstream message that
// opened the circuit, so rejections and the health snapshot can explain WHY
// the circuit is open. Called by the fallback engine right after the matching
// Record*Failure. The message is truncated to 200 chars.
func (h *HealthTracker) RecordCause(name, model, errType, msg string) {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.states[name]; s != nil {
		s.lastErrType, s.lastErrMsg = errType, msg
	}
	if model != "" {
		if s := h.states[name+"|"+model]; s != nil {
			s.lastErrType, s.lastErrMsg = errType, msg
		}
	}
}

// OpenCircuitDescription returns a human-readable description of the currently
// open circuit for (name, model): "<name> circuit open (last error: <type>:
// <msg>, open for another <n>s)". Empty string when no open circuit exists.
// Used to enrich no-route errors so logs carry the upstream cause.
func (h *HealthTracker) OpenCircuitDescription(name, model string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := []string{name}
	if model != "" {
		keys = append(keys, name+"|"+model)
	}
	now := time.Now()
	var best *circuitState
	for _, k := range keys {
		s := h.states[k]
		if s == nil || s.openUntil.IsZero() || !now.Before(s.openUntil) {
			continue
		}
		if best == nil || s.openUntil.After(best.openUntil) {
			best = s
		}
	}
	if best == nil {
		return ""
	}
	remaining := time.Until(best.openUntil).Round(time.Second)
	desc := fmt.Sprintf("%s circuit open, reopens in %s", name, remaining)
	if best.lastErrType != "" || best.lastErrMsg != "" {
		desc += fmt.Sprintf(" (last error: %s: %s", best.lastErrType, best.lastErrMsg)
		if best.openCount > 1 {
			desc += fmt.Sprintf(", open #%d", best.openCount)
		}
		desc += ")"
	}
	return desc
}

// SetMaxEscalation sets the cap for the escalating transient cooldown.
func (h *HealthTracker) SetMaxEscalation(d time.Duration) {
	h.mu.Lock()
	h.maxEscalation = d
	h.mu.Unlock()
}

// SetPersistentCooldown sets the cooldown applied to persistent failures.
func (h *HealthTracker) SetPersistentCooldown(d time.Duration) {
	h.mu.Lock()
	h.persistentCooldown = d
	h.mu.Unlock()
}

// SetRetryAfterBounds sets the clamp range for Retry-After on transient failures.
func (h *HealthTracker) SetRetryAfterBounds(min, max time.Duration) {
	h.mu.Lock()
	h.retryAfterMin = min
	h.retryAfterMax = max
	h.mu.Unlock()
}

// UpdateConfig updates the transient threshold and cooldown (existing signature, used
// by the startup path and the resilience refresh loop).
func (h *HealthTracker) UpdateConfig(failThreshold int, openDuration time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if failThreshold > 0 {
		h.transientThreshold = failThreshold
	}
	if openDuration > 0 {
		h.transientCooldown = openDuration
	}
}
