package provider

import (
	"sync"
	"time"
)

type HealthTracker struct {
	mu            sync.Mutex
	states        map[string]*circuitState
	failThreshold int
	openDuration  time.Duration
}

type circuitState struct {
	consecutiveFails int
	openUntil        time.Time
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
		states:        make(map[string]*circuitState),
		failThreshold: failThreshold,
		openDuration:  openDuration,
	}
}

func (h *HealthTracker) IsHealthy(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.states[name]
	if !ok {
		return true
	}
	if s.openUntil.IsZero() {
		return true
	}
	return time.Now().After(s.openUntil)
}

func (h *HealthTracker) RecordSuccess(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, name)
}

func (h *HealthTracker) RecordFailure(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.states[name]
	if s == nil {
		s = &circuitState{}
		h.states[name] = s
	}
	// If was open (half-open probe failed), re-open from 1
	if !s.openUntil.IsZero() && time.Now().After(s.openUntil) {
		s.consecutiveFails = 1
		s.openUntil = time.Time{}
		return
	}
	s.consecutiveFails++
	if s.consecutiveFails >= h.failThreshold {
		s.openUntil = time.Now().Add(h.openDuration)
	}
}

func (h *HealthTracker) UpdateConfig(failThreshold int, openDuration time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if failThreshold > 0 {
		h.failThreshold = failThreshold
	}
	if openDuration > 0 {
		h.openDuration = openDuration
	}
}
