package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/crosslink/internal/guardrail"
	"github.com/crosslink/internal/model"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// SsrfAllowlistChannel is the Redis pub/sub channel used to invalidate the
// cached allowlist on OTHER instances after a page change. Payload is the new
// row version. Versions are PERSISTENTLY MONOTONIC (reset writes a tombstone
// row instead of deleting), so version-based dedup stays correct across
// generations. Instances without Redis converge via the bounded TTL pull.
const SsrfAllowlistChannel = "crosslink:ssrf-allowlist:changed"

// ssrfAllowlistRefreshTTL bounds how long an instance may serve a stale
// allowlist when no Redis broadcast arrives (or Redis is down). Var only so
// tests can shorten it; production never reassigns it.
var ssrfAllowlistRefreshTTL = 30 * time.Second

// Ssrf allowlist sources, in precedence order: db > env > yaml > strict.
const (
	SsrfSourceDB     = "db"
	SsrfSourceEnv    = "env"
	SsrfSourceYAML   = "yaml"
	SsrfSourceStrict = "strict"
)

// SsrfAllowlistErrorKind classifies apply/reset failures for the HTTP layer:
// "invalid" (400, validation), "conflict" (409, optimistic-lock mismatch),
// "internal" (500, storage failure). A conflict carries the current row so
// the operator can reload and retry.
const (
	SsrfErrInvalid  = "invalid"
	SsrfErrConflict = "conflict"
	SsrfErrInternal = "internal"
)

type SsrfAllowlistError struct {
	Kind           string
	Message        string
	CurrentVersion *int64
	CurrentCIDRs   []string
}

func (e *SsrfAllowlistError) Error() string { return e.Message }

// SsrfAllowlistActor identifies the admin performing a change for the
// same-transaction audit row.
type SsrfAllowlistActor struct {
	UserID    int64
	Username  string
	IP        string
	UserAgent string
}

// SsrfAllowlistStatus is the GET /system/ssrf-allowlist response body: the
// EFFECTIVE policy (memory truth), its source, the DB row version writers
// must present, whether a page override is active, the reset fallback
// preview, the degraded reason (if the last load/refresh failed — memory
// keeps the last-known-good policy then), and cross-instance consistency.
type SsrfAllowlistStatus struct {
	EffectiveCIDRs []string        `json:"effective_cidrs"`
	Source         string          `json:"source"`
	Version        int64           `json:"version"` // expected_version for the next write; 0 = no row yet
	OverrideActive bool            `json:"override_active"`
	Degraded       string          `json:"degraded,omitempty"`
	FallbackCIDRs  []string        `json:"fallback_cidrs"`
	FallbackSource string          `json:"fallback_source"`
	Consistency    SsrfConsistency `json:"consistency"`
}

type SsrfConsistency struct {
	Broadcast     string `json:"broadcast"`      // "redis" | "ttl-only"
	WindowSeconds int    `json:"window_seconds"` // worst-case cross-instance staleness
	// Reauth reports where sensitive-op re-authentication state lives:
	// "redis" (shared across replicas, atomic consume + rate limit) or
	// "local" (node-only tokens/limits — set by the admin handler).
	Reauth string `json:"reauth,omitempty"`
}

type SsrfApplyResult struct {
	Version        int64    `json:"version"`
	EffectiveCIDRs []string `json:"effective_cidrs"`
	Source         string   `json:"source"`
	Broadcast      string   `json:"broadcast"` // "redis" | "failed" (ttl converges)
}

// SsrfAllowlistService owns the hot-reloadable SSRF internal allowlist:
//
//	effective = active DB row (may be the explicit empty list) > env > YAML > strict
//
// The DB row is the single source of truth once written; env/YAML only seed
// the policy when no row exists. RESET DOES NOT DELETE THE ROW — it writes a
// tombstone (removed=true) so the version counter NEVER resets. A physical
// delete would let a stale page holding expected_version=1 from an OLD
// generation overwrite a NEW v1 after recreation (ABA), and would break
// cross-instance version dedup across generations.
//
// "Row absent" (never configured) and "row read failed" are strictly
// distinguished: a failed read never widens the policy — at cold start the
// strict default is published and the fault is surfaced via Status; at
// runtime the last-known-good policy stays active with a degraded marker.
//
// Write path (Apply/Reset): PURE validation first (memory untouched), then
// one transaction that compare-and-swaps the row AND inserts the audit log,
// and only after that transaction commits is the new policy published to
// memory and the invalidation broadcast.
//
// PUBLISH ORDERING: every memory publish goes through publishDBGuarded /
// publishStaticGuarded, which serialize on a mutex and refuse any publish
// whose DB version is not strictly newer than the last one applied. A slow
// Refresh that read an older row can therefore NEVER roll the policy back
// over a newer Apply/Reset publish (and never widen permissions by reviving
// a stale allowlist). TTL recovery is not needed for this invariant.
type SsrfAllowlistService struct {
	db           *gorm.DB
	rdb          *redis.Client
	staticCIDRs  []string
	staticSource string // env | yaml | strict (what a reset falls back to)

	// publishMu serializes publishes; lastAppliedDBVersion makes them
	// monotonic (publishedAny distinguishes "nothing published yet" from
	// "revision 0 was published", so the initial static seed is never
	// mistaken for a stale observation). Guarded by publishMu.
	publishMu            sync.Mutex
	lastAppliedDBVersion int64
	publishedAny         bool

	stateMu sync.RWMutex
	state   SsrfPublishedState
}

type SsrfPublishedState struct {
	Source   string // source of the last successful publish
	Version  int64  // DB row version that produced this publish (0 for pure static)
	Degraded string // non-empty: last load/refresh failed, memory holds last-known-good
}

func NewSsrfAllowlistService(db *gorm.DB, rdb *redis.Client, staticCIDRs []string, staticFromEnv bool) *SsrfAllowlistService {
	src := SsrfSourceStrict
	switch {
	case staticFromEnv && len(staticCIDRs) > 0:
		src = SsrfSourceEnv
	case len(staticCIDRs) > 0:
		src = SsrfSourceYAML
	}
	return &SsrfAllowlistService{
		db:           db,
		rdb:          rdb,
		staticCIDRs:  staticCIDRs,
		staticSource: src,
	}
}

// ── data access ──

// readRow distinguishes "no row (never configured)" (found=false, nil error)
// from a genuine read failure. That distinction is policy-relevant: absence
// falls back to env/YAML, failure never widens.
func (s *SsrfAllowlistService) readRow(q *gorm.DB) (*model.SsrfAllowlistConfig, bool, error) {
	var row model.SsrfAllowlistConfig
	err := q.Where("id = ?", 1).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &row, true, nil
}

func parseCIDRsJSON(raw string) ([]string, error) {
	var cidrs []string
	if err := json.Unmarshal([]byte(raw), &cidrs); err != nil {
		return nil, fmt.Errorf("stored cidrs are not a JSON array: %w", err)
	}
	return cidrs, nil
}

// ── guarded publishes (ordering + monotonic revision) ──

// publishDBGuarded publishes an ACTIVE-row policy, but only if version is
// strictly newer than the last applied DB version. Returns false when the
// publish was (correctly) skipped as stale — e.g. a Refresh that raced a
// newer Apply, or a second instance's refresh that already applied ours.
func (s *SsrfAllowlistService) publishDBGuarded(cidrs []string, version int64) bool {
	prefixes, err := guardrail.ValidateInternalAllowlist(cidrs)
	if err != nil {
		// Defensive: rows are validated before persisting; a row that no
		// longer validates is handled by the caller as invalid-policy, so
		// this branch is unreachable by construction.
		slog.Error("ssrf allowlist: refusing to publish non-validating db row", "version", version, "error", err)
		return false
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.publishedAny && version <= s.lastAppliedDBVersion {
		return false // stale observation: never roll the policy back
	}
	guardrail.ApplyInternalAllowlist(prefixes)
	s.lastAppliedDBVersion = version
	s.publishedAny = true
	s.stateMu.Lock()
	s.state = SsrfPublishedState{Source: SsrfSourceDB, Version: version}
	s.stateMu.Unlock()
	return true
}

// publishStaticGuarded publishes the static (env>YAML>strict) policy as the
// consequence of the DB state observed at observedVersion — a tombstone, or
// a never-configured row (observedVersion 0). Refused when a newer DB
// version was already applied, so a slow stale refresh cannot resurrect the
// static policy over a committed page change.
func (s *SsrfAllowlistService) publishStaticGuarded(observedVersion int64) bool {
	prefixes, err := guardrail.ValidateInternalAllowlist(s.staticCIDRs)
	if err != nil {
		slog.Error("ssrf allowlist: static env/yaml value invalid, applying strict default", "error", err)
		prefixes = nil
	}
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.publishedAny && observedVersion <= s.lastAppliedDBVersion {
		return false
	}
	guardrail.ApplyInternalAllowlist(prefixes)
	s.lastAppliedDBVersion = observedVersion
	s.publishedAny = true
	s.stateMu.Lock()
	s.state = SsrfPublishedState{Source: s.staticSource, Version: observedVersion}
	s.stateMu.Unlock()
	return true
}

func (s *SsrfAllowlistService) setDegraded(reason string) {
	s.stateMu.Lock()
	s.state.Degraded = reason
	s.stateMu.Unlock()
}

func (s *SsrfAllowlistService) clearDegraded() {
	s.stateMu.Lock()
	if s.state.Degraded != "" {
		s.state.Degraded = ""
	}
	s.stateMu.Unlock()
}

// ── lifecycle ──

// LoadColdStart resolves the effective policy at process start. It must run
// BEFORE any request is served. Fail-closed: a DB read failure or an invalid
// stored row publishes the STRICT default (never the possibly-wider env/YAML
// value) and records the reason; the service stays degraded until a refresh
// succeeds.
func (s *SsrfAllowlistService) LoadColdStart(ctx context.Context) {
	row, found, err := s.readRow(s.db.WithContext(ctx))
	switch {
	case err != nil:
		// Fail-closed: strict, and do NOT advance the revision — a later
		// successful read of the same row must still be publishable.
		guardrail.ApplyInternalAllowlist(nil)
		s.stateMu.Lock()
		s.state = SsrfPublishedState{Source: SsrfSourceStrict, Degraded: fmt.Sprintf("cold start: database read failed, strict policy applied: %v", err)}
		s.stateMu.Unlock()
		slog.Error("ssrf allowlist: cold-start DB read failed; STRICT policy applied (internal targets blocked until the DB recovers)", "error", err)
		return
	case found && !row.Removed:
		cidrs, perr := parseCIDRsJSON(row.CIDRsJSON)
		if perr != nil {
			cidrs = nil
			err = perr
		}
		if err == nil {
			if _, verr := guardrail.ValidateInternalAllowlist(cidrs); verr != nil {
				err = verr
			}
		}
		if err != nil {
			guardrail.ApplyInternalAllowlist(nil)
			s.stateMu.Lock()
			s.state = SsrfPublishedState{Source: SsrfSourceStrict, Degraded: fmt.Sprintf("cold start: stored allowlist invalid, strict policy applied: %v", err)}
			s.stateMu.Unlock()
			slog.Error("ssrf allowlist: stored DB allowlist invalid; STRICT policy applied instead of falling back to env", "version", row.Version, "error", err)
			return
		}
		if s.publishDBGuarded(cidrs, row.Version) {
			slog.Info("ssrf allowlist: loaded page-configured allowlist", "cidrs", cidrs, "version", row.Version)
		}
	case found && row.Removed:
		// Tombstone: override removed; fall back to static policy.
		s.publishStaticGuarded(row.Version)
		slog.Info("ssrf allowlist: page override removed (tombstone); static policy active", "source", s.staticSource, "version", row.Version)
	default:
		s.publishStaticGuarded(0)
		slog.Info("ssrf allowlist: no page override; static policy active", "source", s.staticSource, "cidrs", s.staticCIDRs)
	}
}

// Start launches the bounded-TTL refresher and, when Redis is configured,
// the invalidation subscriber. Blocks until ctx is cancelled; run in a
// goroutine.
func (s *SsrfAllowlistService) Start(ctx context.Context) {
	ticker := time.NewTicker(ssrfAllowlistRefreshTTL)
	defer ticker.Stop()
	refresh := func() {
		if err := s.Refresh(ctx); err != nil {
			slog.Warn("ssrf allowlist: periodic refresh failed; last-known-good policy stays active", "error", err)
		}
	}
	if s.rdb == nil {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}
	// Register the FIRST subscription SYNCHRONOUSLY on the caller's
	// goroutine: a broadcast published immediately after Start() begins can
	// no longer race the subscriber's registration (the TTL pull remains
	// the bounded backstop, but a fast writer right after startup — or a
	// test — must not depend on it). Reconnects stay in the loop goroutine.
	sub := s.rdb.Subscribe(ctx, SsrfAllowlistChannel)
	slog.Info("ssrf allowlist: subscribed to invalidation broadcast", "channel", SsrfAllowlistChannel)
	subDone := make(chan struct{})
	go func() {
		defer close(subDone)
		for {
			ch := sub.Channel()
		loop:
			for {
				select {
				case <-ctx.Done():
					sub.Close()
					return
				case msg, ok := <-ch:
					if !ok {
						break loop // reconnect
					}
					if v, err := strconv.ParseInt(msg.Payload, 10, 64); err == nil && v > 0 {
						s.stateMu.RLock()
						cur := s.state
						s.stateMu.RUnlock()
						// Versions are persistent and monotonic (reset writes
						// a tombstone), so this dedup is generation-safe.
						if cur.Source == SsrfSourceDB && cur.Version >= v {
							continue // already at/behind this version
						}
					}
					// v == 0 (legacy) or unknown payload: always refresh.
					refresh()
				}
			}
			sub.Close()
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			sub = s.rdb.Subscribe(ctx, SsrfAllowlistChannel)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			<-subDone
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// Refresh re-reads the DB row (TTL pull / broadcast-triggered) and publishes
// through the ordering guard. Runtime rule: on read failure or an invalid
// row the last-known-good policy stays active and the degraded marker is
// set; the policy never widens due to a failure. A refresh that observed an
// older row than already applied is a no-op — the guard refuses it, so a
// slow refresh can never roll a newer policy back.
func (s *SsrfAllowlistService) Refresh(ctx context.Context) error {
	row, found, err := s.readRow(s.db.WithContext(ctx))
	if err != nil {
		s.setDegraded(fmt.Sprintf("allowlist refresh failed, last-known-good policy kept: %v", err))
		return err
	}
	switch {
	case found && !row.Removed:
		cidrs, perr := parseCIDRsJSON(row.CIDRsJSON)
		if perr != nil {
			s.setDegraded(fmt.Sprintf("stored allowlist unreadable, last-known-good policy kept: %v", perr))
			return perr
		}
		if _, verr := guardrail.ValidateInternalAllowlist(cidrs); verr != nil {
			s.setDegraded(fmt.Sprintf("stored allowlist invalid, last-known-good policy kept: %v", verr))
			return verr
		}
		s.clearDegraded()
		if s.publishDBGuarded(cidrs, row.Version) {
			slog.Info("ssrf allowlist: applied refreshed policy", "version", row.Version, "cidrs", cidrs)
		}
	case found && row.Removed:
		s.clearDegraded()
		if s.publishStaticGuarded(row.Version) {
			slog.Info("ssrf allowlist: override removed (tombstone observed); fell back to static policy", "source", s.staticSource, "version", row.Version)
		}
	default:
		s.clearDegraded()
		s.publishStaticGuarded(0)
	}
	return nil
}

// broadcast notifies other instances. Best effort: a failure is reported to
// the caller (surfaced in the apply result) and the TTL pull converges.
func (s *SsrfAllowlistService) broadcast(ctx context.Context, version int64) bool {
	if s.rdb == nil {
		return false
	}
	if err := s.rdb.Publish(ctx, SsrfAllowlistChannel, strconv.FormatInt(version, 10)).Err(); err != nil {
		slog.Error("ssrf allowlist: invalidation broadcast failed; instances converge via TTL pull", "error", err)
		return false
	}
	return true
}

// ── write paths ──

type ssrfTxOutcome struct {
	newVersion int64
	oldCIDRs   []string
	oldRemoved bool
	existed    bool
}

func conflictErr(row *model.SsrfAllowlistConfig) *SsrfAllowlistError {
	e := &SsrfAllowlistError{Kind: SsrfErrConflict}
	if row != nil {
		v := row.Version
		e.CurrentVersion = &v
		if !row.Removed {
			if c, perr := parseCIDRsJSON(row.CIDRsJSON); perr == nil {
				e.CurrentCIDRs = c
			}
		}
	}
	return e
}

// applyTx runs the compare-and-swap row write plus the audit insert in ONE
// transaction. The caller has already validated cidrs (pure validation; the
// memory policy is untouched until this transaction commits). Writing over a
// tombstone is allowed with its CURRENT version — that is the anti-ABA rule:
// a stale page must present the version of the GENERATION IT READ, and
// versions never go backwards.
func (s *SsrfAllowlistService) applyTx(ctx context.Context, cidrsJSON string, expected int64, actor SsrfAllowlistActor) (*ssrfTxOutcome, *SsrfAllowlistError) {
	var out ssrfTxOutcome
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, found, err := s.readRow(tx)
		if err != nil {
			return err
		}
		out.existed = found
		now := time.Now().UTC()
		if found {
			out.oldRemoved = row.Removed
			if c, perr := parseCIDRsJSON(row.CIDRsJSON); perr == nil {
				out.oldCIDRs = c
			}
			if row.Version != expected {
				return conflictErr(row)
			}
			out.newVersion = row.Version + 1
			res := tx.Model(&model.SsrfAllowlistConfig{}).
				Where("id = ? AND version = ?", 1, expected).
				Updates(map[string]any{
					"cidrs":           cidrsJSON,
					"version":         out.newVersion,
					"removed":         false,
					"updated_by":      actor.UserID,
					"updated_by_name": actor.Username,
					"updated_at":      now,
				})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return conflictErr(row)
			}
		} else {
			if expected != 0 {
				z := int64(0)
				return &SsrfAllowlistError{Kind: SsrfErrConflict, CurrentVersion: &z}
			}
			out.newVersion = 1
			if err := tx.Create(&model.SsrfAllowlistConfig{
				ID:            1,
				CIDRsJSON:     cidrsJSON,
				Version:       1,
				Removed:       false,
				UpdatedBy:     actor.UserID,
				UpdatedByName: actor.Username,
				UpdatedAt:     now,
			}).Error; err != nil {
				return err
			}
		}
		detail, _ := json.Marshal(map[string]any{
			"from":    out.oldCIDRs,
			"to":      json.RawMessage(cidrsJSON),
			"removed": false,
			"version": out.newVersion,
		})
		return tx.Create(&model.AuditLog{
			UserID:       actor.UserID,
			Username:     actor.Username,
			Action:       "system:update_ssrf_allowlist",
			ResourceType: "setting",
			ResourceID:   "ssrf_allowlist",
			ResourceName: "SSRF internal allowlist",
			Detail:       detail,
			IPAddress:    actor.IP,
			UserAgent:    actor.UserAgent,
			Status:       "success",
			CreatedAt:    now,
		}).Error
	})
	if txErr != nil {
		var se *SsrfAllowlistError
		if errors.As(txErr, &se) {
			return nil, se
		}
		return nil, &SsrfAllowlistError{Kind: SsrfErrInternal, Message: fmt.Sprintf("persist allowlist: %v", txErr)}
	}
	return &out, nil
}

// Apply validates and persists a new allowlist (empty slice = EXPLICIT
// strict) and publishes it only after the transaction committed.
func (s *SsrfAllowlistService) Apply(ctx context.Context, cidrs []string, expected int64, actor SsrfAllowlistActor) (*SsrfApplyResult, *SsrfAllowlistError) {
	prefixes, err := guardrail.ValidateInternalAllowlist(cidrs)
	if err != nil {
		return nil, &SsrfAllowlistError{Kind: SsrfErrInvalid, Message: err.Error()}
	}
	canonical := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		canonical = append(canonical, p.String())
	}
	raw, merr := json.Marshal(canonical)
	if merr != nil {
		return nil, &SsrfAllowlistError{Kind: SsrfErrInternal, Message: fmt.Sprintf("marshal cidrs: %v", merr)}
	}
	out, serr := s.applyTx(ctx, string(raw), expected, actor)
	if serr != nil {
		return nil, serr
	}
	// Transaction committed: NOW publish memory (ordering-guarded atomic
	// swap + idle-conn observers) and notify other instances.
	s.publishDBGuarded(canonical, out.newVersion)
	res := &SsrfApplyResult{
		Version:        out.newVersion,
		EffectiveCIDRs: canonical,
		Source:         SsrfSourceDB,
		Broadcast:      "failed",
	}
	if s.broadcast(ctx, out.newVersion) {
		res.Broadcast = "redis"
	} else if s.rdb == nil {
		res.Broadcast = "ttl-only"
	}
	slog.Info("ssrf allowlist: page update applied", "version", out.newVersion, "cidrs", canonical, "operator", actor.Username)
	return res, nil
}

// Reset tombstones the page override (falling back to env > YAML > strict)
// WITHOUT deleting the row: the version counter keeps increasing so stale
// pages from a previous generation can never win a compare-and-swap against
// a newer one (anti-ABA). An explicit empty Apply stores a strict OVERRIDE
// instead — the two are never conflated.
//
// Idempotence: reset on an already-tombstoned row with the matching version
// is a no-op success (no bump, no audit, no republish). expected==0 with no
// row at all is likewise a no-op.
func (s *SsrfAllowlistService) Reset(ctx context.Context, expected int64, actor SsrfAllowlistActor) (*SsrfApplyResult, *SsrfAllowlistError) {
	deleted := false
	var out ssrfTxOutcome
	txErr := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row, found, err := s.readRow(tx)
		if err != nil {
			return err
		}
		out.existed = found
		if !found {
			if expected == 0 {
				return nil // idempotent no-op: nothing ever configured
			}
			z := int64(0)
			return &SsrfAllowlistError{Kind: SsrfErrConflict, CurrentVersion: &z}
		}
		out.oldRemoved = row.Removed
		if c, perr := parseCIDRsJSON(row.CIDRsJSON); perr == nil {
			out.oldCIDRs = c
		}
		if row.Version != expected {
			return conflictErr(row)
		}
		if row.Removed {
			return nil // already tombstoned at this version: idempotent no-op
		}
		out.newVersion = row.Version + 1
		res := tx.Model(&model.SsrfAllowlistConfig{}).
			Where("id = ? AND version = ?", 1, expected).
			Updates(map[string]any{
				"cidrs":           "[]",
				"version":         out.newVersion,
				"removed":         true,
				"updated_by":      actor.UserID,
				"updated_by_name": actor.Username,
				"updated_at":      time.Now().UTC(),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return conflictErr(row)
		}
		deleted = true
		detail, _ := json.Marshal(map[string]any{
			"from":            out.oldCIDRs,
			"to":              nil,
			"removed":         true,
			"fallback_source": s.staticSource,
			"version":         out.newVersion,
		})
		return tx.Create(&model.AuditLog{
			UserID:       actor.UserID,
			Username:     actor.Username,
			Action:       "system:reset_ssrf_allowlist",
			ResourceType: "setting",
			ResourceID:   "ssrf_allowlist",
			ResourceName: "SSRF internal allowlist",
			Detail:       detail,
			IPAddress:    actor.IP,
			UserAgent:    actor.UserAgent,
			Status:       "success",
			CreatedAt:    time.Now().UTC(),
		}).Error
	})
	if txErr != nil {
		var se *SsrfAllowlistError
		if errors.As(txErr, &se) {
			return nil, se
		}
		return nil, &SsrfAllowlistError{Kind: SsrfErrInternal, Message: fmt.Sprintf("reset allowlist: %v", txErr)}
	}
	if !deleted {
		// Idempotent no-op: report the CURRENT policy without republishing
		// (a republish would clear a legitimately set degraded marker).
		s.stateMu.RLock()
		src := s.state.Source
		s.stateMu.RUnlock()
		prefixes := guardrail.InternalAllowlistSnapshot()
		eff := make([]string, 0, len(prefixes))
		for _, p := range prefixes {
			eff = append(eff, p.String())
		}
		return &SsrfApplyResult{Version: expected, EffectiveCIDRs: eff, Source: src, Broadcast: "ttl-only"}, nil
	}
	// Tombstone committed: fall back to the static policy, ordering-guarded
	// so this cannot roll back over an even newer concurrent write.
	s.publishStaticGuarded(out.newVersion)
	res := &SsrfApplyResult{
		Version:        out.newVersion,
		EffectiveCIDRs: s.staticCIDRs,
		Source:         s.staticSource,
		Broadcast:      "failed",
	}
	if s.broadcast(ctx, out.newVersion) {
		res.Broadcast = "redis"
	} else if s.rdb == nil {
		res.Broadcast = "ttl-only"
	}
	slog.Info("ssrf allowlist: page override tombstoned; static policy active", "source", s.staticSource, "version", out.newVersion, "operator", actor.Username)
	return res, nil
}

// Status reports the effective policy (memory truth), the DB row version a
// writer must present (0 = no row yet; tombstones carry their version), the
// reset fallback preview, the degraded marker if set, and cross-instance
// consistency information.
func (s *SsrfAllowlistService) Status(ctx context.Context) (*SsrfAllowlistStatus, error) {
	row, found, err := s.readRow(s.db.WithContext(ctx))
	if err != nil {
		// Distinguish a status-read failure from policy state: the effective
		// policy still comes from memory; the error is reported alongside.
		slog.Warn("ssrf allowlist: status DB read failed", "error", err)
	}
	s.stateMu.RLock()
	st := s.state
	s.stateMu.RUnlock()
	prefixes := guardrail.InternalAllowlistSnapshot()
	eff := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		eff = append(eff, p.String())
	}
	status := &SsrfAllowlistStatus{
		EffectiveCIDRs: eff,
		Source:         st.Source,
		Degraded:       st.Degraded,
		FallbackCIDRs:  s.staticCIDRs,
		FallbackSource: s.staticSource,
		Consistency: SsrfConsistency{
			Broadcast:     "ttl-only",
			WindowSeconds: int(ssrfAllowlistRefreshTTL.Seconds()),
		},
	}
	if s.rdb != nil {
		status.Consistency.Broadcast = "redis"
	}
	if found {
		status.Version = row.Version
		status.OverrideActive = !row.Removed
	}
	if err != nil && status.Degraded == "" {
		status.Degraded = fmt.Sprintf("status read failed (effective policy unchanged): %v", err)
	}
	return status, nil
}
