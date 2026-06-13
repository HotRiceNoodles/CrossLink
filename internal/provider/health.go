package provider

import (
	"sync"
	"time"
)

type circuitKind int8

const (
	kindTransient circuitKind = iota
	kindPersistent
)

type circuitState struct {
	kind             circuitKind
	consecutiveFails int
	openUntil        time.Time
	probeInFlight    bool // half-open single-flight gate (C2)
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
		s.openUntil = now.Add(h.clampRetryAfter(retryAfter))
	}
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
