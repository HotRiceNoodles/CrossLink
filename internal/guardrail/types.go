package guardrail

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type Direction string

const (
	DirectionRequest  Direction = "request"
	DirectionResponse Direction = "response"
	DirectionBoth     Direction = "both"
)

type GuardrailResult struct {
	Blocked        bool   `json:"blocked"`
	RuleName       string `json:"rule_name,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Severity       string `json:"severity"`
	MaskedContent  string `json:"masked_content,omitempty"`
	ContentSnippet string `json:"content_snippet,omitempty"`
	// MaskedSpans are replacements relative to the content passed to Check.
	// Engines that can locate their matches should fill these instead of (or in
	// addition to) the whole-string MaskedContent, so callers that know the
	// field layout of the content can mask each field in place without
	// collapsing structure (roles, arrays, media blocks).
	MaskedSpans []Span `json:"masked_spans,omitempty"`
	// Scores carries the engine's structured decision numbers (aggregate
	// score, strongest single-signal weight, flag-style values as 0/1). It is
	// the machine-readable counterpart of the human-facing Reason: adaptive
	// tuning consumes THIS, never parses the Reason text. Optional — engines
	// without continuous scoring leave it nil.
	Scores map[string]float64 `json:"scores,omitempty"`
}

// Span is one masked region of checked content: [Start, End) are byte offsets
// relative to the content given to the engine, Replacement is what replaces it.
type Span struct {
	Start       int    `json:"start"`
	End         int    `json:"end"`
	Replacement string `json:"replacement"`
	// Reversible is internal only; original values never enter API results or events.
	Reversible bool `json:"-"`
	// Purpose distinguishes span producers on the shared MaskedSpans channel
	// (freeze C1): secret masking (PII/credentials) vs sanitize evidence.
	// Empty = secret_mask, the pre-existing semantics; json:"-" keeps the
	// wire format of existing paths unchanged.
	Purpose SpanPurpose `json:"-"`
}

// SpanPurpose labels why a span exists (freeze C1). Existing PII/mask paths
// keep their behavior; sanitize evidence is produced only by kb_poisoning
// (M1, commercial).
type SpanPurpose string

const (
	SpanPurposeSecretMask       SpanPurpose = "secret_mask"
	SpanPurposeSanitizeEvidence SpanPurpose = "sanitize_evidence"
)

type GuardrailEngine interface {
	Name() string
	Check(ctx context.Context, content string, direction Direction, model string) (*GuardrailResult, error)
}

type EngineCloser interface {
	Close()
}

// ConfigTransformFunc rewrites a rule's Config JSON right before engine
// construction (adaptive-tuning seam, plan 2026-11-20 v1.7 D1/D2). It runs
// once per engine build — engines are cached per rule, so the hot check path
// never pays for it. Community never injects one (identity); the commercial
// edition injects the threshold-multiplier rewrite. An error surfaces as an
// engine-construction failure and follows the existing engine-error policy.
// Implementations must touch ONLY whitelisted threshold-like leaf fields —
// secrets, pattern lists and toggles must survive byte-identical.
type ConfigTransformFunc func(ruleID int64, ruleType string, config json.RawMessage) (json.RawMessage, error)

type GuardrailRule struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	OrgID       *int64         `gorm:"index" json:"org_id,omitempty"`
	Name        string         `gorm:"size:255;not null" json:"name"`
	Type        string         `gorm:"size:50;not null" json:"type"`
	Direction   string         `gorm:"size:10;not null" json:"direction"`
	Enabled     bool           `gorm:"default:true" json:"enabled"`
	Config      datatypes.JSON `gorm:"type:jsonb;not null" json:"config"`
	Severity    string         `gorm:"size:20;default:'medium'" json:"severity"`
	Action      string         `gorm:"size:20;default:'block'" json:"action"`
	ModelFilter *string        `gorm:"type:text" json:"model_filter"`
	// Version is the per-rule monotonic version maintained by the versioned
	// write path (create/update/delete/rollback). Rows predating that path
	// start at 1 via the rule-versions migrations, which also snapshot them
	// into guardrail_rule_versions (change_kind='migrate').
	Version   int64     `gorm:"not null;default:1" json:"version"`
	CreatedAt time.Time `gorm:"not null;default:now()" json:"created_at"`
	UpdatedAt time.Time `gorm:"not null;default:now()" json:"updated_at"`
}

func (GuardrailRule) TableName() string { return "guardrail_rules" }

// GuardrailRuleVersion is one append-only history row: the full snapshot of a
// rule as of a version. Writes are INSERT-only (create/update/delete/rollback
// plus the one-time 'migrate' backfill); no code path updates or deletes
// rows. Config may contain sensitive values — every READ path goes through
// the admin sanitizers; raw Config is consumed only by the rollback write
// path, which carries the same permissions and org scoping as rule updates.
type GuardrailRuleVersion struct {
	ID          int64          `gorm:"primaryKey" json:"id"`
	RuleID      int64          `gorm:"index;uniqueIndex:uq_rule_versions,priority:1" json:"rule_id"`
	Version     int64          `gorm:"uniqueIndex:uq_rule_versions,priority:2" json:"version"`
	OrgID       *int64         `gorm:"index" json:"org_id,omitempty"`
	Name        string         `gorm:"size:255;not null" json:"name"`
	Type        string         `gorm:"size:50;not null" json:"type"`
	Direction   string         `gorm:"size:10;not null" json:"direction"`
	Enabled     bool           `json:"enabled"`
	Config      datatypes.JSON `json:"config"`
	Severity    string         `gorm:"size:20" json:"severity"`
	Action      string         `gorm:"size:20" json:"action"`
	ModelFilter *string        `gorm:"type:text" json:"model_filter"`
	// ChangeKind: create | update | delete | rollback | migrate
	ChangeKind string    `gorm:"size:20;not null" json:"change_kind"`
	Actor      string    `gorm:"size:64;not null;default:''" json:"actor"`
	CreatedAt  time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (GuardrailRuleVersion) TableName() string { return "guardrail_rule_versions" }

// MatchedRule attributes one participating rule in a check decision: the
// rule's ID, the version of the cached copy that actually produced the
// decision, and the action applied. Only ids/versions/actions — nothing
// content-bearing — so it is safe for events, alert logs and audit output.
type MatchedRule struct {
	RuleID  int64  `json:"rule_id"`
	Version int64  `json:"version"`
	Action  string `json:"action"`
}

type CheckRequest struct {
	Content   string
	Direction Direction
	Model     string
	APIKeyID  int64
	TeamID    int64
	OrgID     int64
	// Prompt carries the original user input (last user message) for
	// DirectionResponse checks. Context-aware engines (e.g. Qwen3Guard
	// response moderation) judge the assistant output against it; engines
	// that don't need it ignore it. Empty = unknown; handlers set it on a
	// best-effort basis.
	Prompt string
}

type CheckResponse struct {
	Blocked       bool   `json:"blocked"`
	RuleName      string `json:"rule_name,omitempty"`
	Reason        string `json:"reason,omitempty"`
	MaskedContent string `json:"masked_content,omitempty"`
	Action        string `json:"action,omitempty"`
	// MaskedSpans carries the composed span replacements (byte offsets into
	// the checked content) so protocol-aware callers can mask fields in place.
	MaskedSpans []Span `json:"masked_spans,omitempty"`
	// Truncated reports that the checked content exceeded maxCheckContent and
	// only its prefix was inspected. A pass with Truncated=true is partial
	// coverage, never a clean pass.
	Truncated bool `json:"truncated,omitempty"`
	// Matched attributes every rule that actually participated in the
	// decision (evaluated and matched, up to and including the rule that
	// produced the blocking return) as (rule_id, version, action). Rules
	// evaluated after the blocking return did not participate and are absent.
	Matched []MatchedRule `json:"matched,omitempty"`
}

type GuardrailEvent struct {
	RuleID         int64
	RuleName       string
	EngineType     string
	Direction      string
	Severity       string
	Action         string // pass/blocked/mask/log/error
	Reason         string
	Model          string
	ContentPreview string
	APIKeyID       int64
	TeamID         int64
	OrgID          int64 // tenant attribution; 0 = unattributed (admin/internal)
	Timestamp      time.Time
	AgentType      string // from CheckContext, set by agent_fingerprint engine
	// EventID is the stable identity of this single event, assigned once in
	// GuardrailService.emitEvent (UUIDv7, time-ordered). Every handler that
	// persists the event (alert logs, compliance incidents) records the SAME
	// id, so downstream consumers dedupe and cross-reference one event across
	// tables by id — not by (timestamp, reason) heuristics.
	EventID string `json:"event_id,omitempty"`
	// Scores mirrors GuardrailResult.Scores onto the event channel (see there).
	// Consumed by adaptive tuning decisions; never parsed out of Reason.
	Scores map[string]float64 `json:"scores,omitempty"`
	// RuleVersion is the version of THIS event's rule — the cached copy that
	// produced the result (per-instance view; propagation is TTL/broadcast
	// bounded, never claimed globally atomic).
	RuleVersion int64 `json:"rule_version,omitempty"`
	// Matched, when set on a decision event, carries the full participating
	// (rule_id, version, action) list so a single record attributes every
	// contributing rule — never the last hit smeared across all rules.
	Matched []MatchedRule `json:"matched,omitempty"`
}

type EventHandler func(ctx context.Context, event GuardrailEvent)

type ModelGuardrailConfig struct {
	Enabled        bool     `json:"enabled"`
	RequestChecks  []string `json:"request_checks"`
	ResponseChecks []string `json:"response_checks"`
}

// EngineDeps holds infrastructure dependencies for V2 guardrail engines.
// All fields are optional — engines must check for nil before use.
type EngineDeps struct {
	Redis            redis.Cmdable // optional, nil when Redis is unavailable
	RedisClient      *redis.Client // optional, full client for Pub/Sub; nil when unavailable or Cmdable-only
	DB               *gorm.DB      // optional, nil when DB is unavailable
	IsDebugEnabled   func() bool   // optional, nil = false; reads debug.Store.IsEnabled()
	CheckLicenseTier func() string // optional, nil = "community"; returns "community"/"pro"/"enterprise"
}

// CheckContext is a mutable carrier for inter-engine state within a single Check call.
// Passed via context (CtxKeyCheck), engines read/write through the shared pointer.
type CheckContext struct {
	AgentType string            // set by agent_fingerprint engine
	RiskLevel string            // set by agent_fingerprint engine
	Metadata  map[string]string // extensible field for inter-engine data
}

// CtxKey is the type for guardrail context keys.
type CtxKey string

const (
	CtxKeyUserAgent CtxKey = "guardrail_user_agent"
	CtxKeyHeaders   CtxKey = "guardrail_headers"
	CtxKeyCheck     CtxKey = "guardrail_check" // *CheckContext pointer
)
