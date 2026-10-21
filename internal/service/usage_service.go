package service

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type UsageService struct {
	repo              *repository.UsageLogRepo
	contentLogEnabled atomic.Bool
	middlewareLogCfg  atomic.Value // *MiddlewareLogConfig
}

type MiddlewareLogConfig struct {
	AuthFailure        bool `json:"auth_failure"`
	Forbidden          bool `json:"forbidden"`
	RateLimit          bool `json:"rate_limit"`
	BadRequest         bool `json:"bad_request"`
	BudgetExceeded     bool `json:"budget_exceeded"`
	NotFound           bool `json:"not_found"`
	ServiceUnavailable bool `json:"service_unavailable"`
	ServerError        bool `json:"server_error"`
}

func (s *UsageService) SetMiddlewareLogConfig(cfg *MiddlewareLogConfig) {
	s.middlewareLogCfg.Store(cfg)
}

func (s *UsageService) GetMiddlewareLogConfig() MiddlewareLogConfig {
	v := s.middlewareLogCfg.Load()
	if v == nil {
		return MiddlewareLogConfig{
			AuthFailure: true, Forbidden: true, RateLimit: true,
			BadRequest: true, BudgetExceeded: true, NotFound: true,
			ServiceUnavailable: true, ServerError: true,
		}
	}
	return *v.(*MiddlewareLogConfig)
}

func (s *UsageService) IsMiddlewareLogEnabled(errorType string) bool {
	v := s.middlewareLogCfg.Load()
	if v == nil {
		return true
	}
	cfg := v.(*MiddlewareLogConfig)
	switch errorType {
	case "auth_failure":
		return cfg.AuthFailure
	case "forbidden":
		return cfg.Forbidden
	case "rate_limit":
		return cfg.RateLimit
	case "bad_request":
		return cfg.BadRequest
	case "budget_exceeded":
		return cfg.BudgetExceeded
	case "not_found":
		return cfg.NotFound
	case "service_unavailable":
		return cfg.ServiceUnavailable
	default:
		return true
	}
}

func NewUsageService(repo *repository.UsageLogRepo) *UsageService {
	return &UsageService{repo: repo}
}

func (s *UsageService) SetContentLogEnabled(enabled bool) {
	s.contentLogEnabled.Store(enabled)
}

func (s *UsageService) IsContentLogEnabled() bool {
	return s.contentLogEnabled.Load()
}

type UsageEntry struct {
	// EventID is the stable idempotency key of this billing event. Minted
	// exactly once at submit time (handler.submitUsageEntryFunc) and
	// serialized into the disk spool, so a replay of an already-committed
	// row collides on the usage_logs unique index instead of double-billing.
	// Empty for legacy direct-Log callers (admin playground), which have no
	// replay path and fall back to a one-shot UUID inside buildUsageLog.
	EventID        string
	// GinRequestID carries the gateway request id (the SAME id the budget
	// reserve/settle markers and the request log line use). Persisted into
	// usage_logs.request_id so a row is AUDITABLY joinable to its marks,
	// ledger tasks and log lifecycle (r47 forensics 2026-10-21: the old
	// per-row uuid[:8] lived in a different id space, making per-request
	// miss/double-billing checks impossible). Empty for callers without a
	// request context (probes) — buildUsageLog falls back to the uuid.
	GinRequestID   string
	RouteType      string
	ModelRequested string
	ModelUsed      string
	ProviderID     int64
	APIKeyID       int64
	TeamID         int64
	OrgID          int64
	InputTokens    int
	OutputTokens   int
	InputPrice     float64
	OutputPrice    float64
	Currency       string
	LatencyMs      int64
	FirstTokenMs   int64
	StatusCode     int
	ErrorType      string
	// Error observability L1: sanitized error text + upstream status/code.
	// UpstreamStatus 0 = gateway-side rejection, not an upstream response.
	ErrorMessage      string
	UpstreamStatus    int
	UpstreamErrorCode string
	// Error observability L1.5: upstream error.type/param and the gateway
	// stage the request died at.
	UpstreamErrorType  string
	UpstreamErrorParam string
	ErrorStage         string
	// Attempts is the serialized fallback timeline; nil on clean single-attempt
	// successes (error observability L2).
	Attempts           datatypes.JSON
	UserMessage        string
	ModelResponse      string
	FallbackCount      int
	RetryCount         int
	GuardrailTriggered bool
	GuardrailRule      string
	GuardrailTruncated bool
	AgentType          string
	SecurityEvents     datatypes.JSON
	CacheHit           bool
	ReasoningTokens    int
	CacheReadTokens    int
	SessionID          string
	TemplateID         *int64 // prompt template that assembled this request (nil = none)
	ImageCount         int
	ImageSize          string
	ImageQuality       string
	PrecomputedCost    float64
	PriceMultiplier    float64 // key's price multiplier (1.0 = no markup)
	// Context quality analysis fields — nil = unanalyzed (cache-hit or analysis failure).
	SystemTokens         *int
	HistoryTokens        *int
	QuestionTokens       *int
	ToolTokens           *int
	ToolOutputTokens     *int
	ContextWindow        *int
	ContextUtilizationBp *int
	AnalysisFlags        *int
	ContextSnapshot      datatypes.JSON
}

func (e *UsageEntry) cost() float64 {
	if e.PrecomputedCost > 0 {
		return e.PrecomputedCost
	}
	return e.InputPrice*float64(e.InputTokens)/1000 + e.OutputPrice*float64(e.OutputTokens)/1000
}

func buildUsageLog(entry *UsageEntry) *model.UsageLog {
	mult := entry.PriceMultiplier
	if mult <= 0 {
		mult = 1.0
	}
	upstreamCost := entry.cost()
	// Stable billing-event id (F34): prefer the one minted at submit time so
	// retries/replays of the SAME entry dedupe on the unique index; only
	// direct-Log callers without a submit path mint here (one-shot, no
	// replay, same effective behavior as before).
	eventID := entry.EventID
	if eventID == "" {
		eventID = uuid.New().String()
	}
	log := &model.UsageLog{
		RequestID: firstNonEmpty(entry.GinRequestID, uuid.New().String()),
		EventID:            &eventID,
		RouteType:          entry.RouteType,
		ModelRequested:     entry.ModelRequested,
		ModelUsed:          entry.ModelUsed,
		InputTokens:        entry.InputTokens,
		OutputTokens:       entry.OutputTokens,
		Cost:               upstreamCost,
		BillableCost:       upstreamCost * mult,
		Currency:           model.ValidCurrency(entry.Currency),
		LatencyMs:          int(entry.LatencyMs),
		StatusCode:         entry.StatusCode,
		ErrorType:          entry.ErrorType,
		FallbackCount:      entry.FallbackCount,
		RetryCount:         entry.RetryCount,
		GuardrailTriggered: entry.GuardrailTriggered,
		GuardrailRule:      entry.GuardrailRule,
		GuardrailTruncated: entry.GuardrailTruncated,
		CacheHit:           entry.CacheHit,
		AgentType:          entry.AgentType,
		SecurityEvents:     entry.SecurityEvents,
		ReasoningTokens:    entry.ReasoningTokens,
		CacheReadTokens:    entry.CacheReadTokens,
		SessionID:          entry.SessionID,
	}
	if entry.FirstTokenMs > 0 {
		ms := int(entry.FirstTokenMs)
		log.FirstTokenMs = &ms
	}
	if entry.ProviderID > 0 {
		log.ProviderID = &entry.ProviderID
	}
	if entry.APIKeyID > 0 {
		log.APIKeyID = &entry.APIKeyID
	}
	if entry.TeamID > 0 {
		log.TeamID = &entry.TeamID
	}
	if entry.OrgID > 0 {
		log.OrgID = &entry.OrgID
	}
	if entry.TemplateID != nil {
		log.TemplateID = entry.TemplateID
	}
	if entry.UserMessage != "" {
		log.UserMessage = &entry.UserMessage
	}
	if entry.ModelResponse != "" {
		log.ModelResponse = &entry.ModelResponse
	}
	if entry.ErrorMessage != "" {
		log.ErrorMessage = &entry.ErrorMessage
	}
	if entry.UpstreamStatus > 0 {
		us := entry.UpstreamStatus
		log.UpstreamStatus = &us
	}
	if entry.UpstreamErrorCode != "" {
		log.UpstreamErrorCode = &entry.UpstreamErrorCode
	}
	if entry.UpstreamErrorType != "" {
		log.UpstreamErrorType = &entry.UpstreamErrorType
	}
	if entry.UpstreamErrorParam != "" {
		log.UpstreamErrorParam = &entry.UpstreamErrorParam
	}
	if entry.ErrorStage != "" {
		log.ErrorStage = &entry.ErrorStage
	}
	if len(entry.Attempts) > 0 {
		log.Attempts = entry.Attempts
	}
	if entry.ImageCount > 0 {
		cnt := int64(entry.ImageCount)
		log.ImageCount = &cnt
	}
	if entry.ImageSize != "" {
		log.ImageSize = &entry.ImageSize
	}
	if entry.ImageQuality != "" {
		log.ImageQuality = &entry.ImageQuality
	}
	log.SystemTokens = entry.SystemTokens
	log.HistoryTokens = entry.HistoryTokens
	log.QuestionTokens = entry.QuestionTokens
	log.ToolTokens = entry.ToolTokens
	log.ToolOutputTokens = entry.ToolOutputTokens
	log.ContextWindow = entry.ContextWindow
	log.ContextUtilizationBp = entry.ContextUtilizationBp
	log.AnalysisFlags = entry.AnalysisFlags
	log.ContextSnapshot = entry.ContextSnapshot
	return log
}

func (s *UsageService) Log(ctx context.Context, entry *UsageEntry) {
	log := buildUsageLog(entry)
	if err := s.repo.Create(ctx, log); err != nil {
		slog.Error("failed to write usage log", "error", err)
	}
}

// LogChecked is Log with the DB result surfaced: nil means the row is
// durably stored. The usage spill replay relies on this ack — a row leaves
// the spool only when this returns nil.
func (s *UsageService) LogChecked(ctx context.Context, entry *UsageEntry) error {
	log := buildUsageLog(entry)
	if err := s.repo.Create(ctx, log); err != nil {
		slog.Error("failed to write usage log", "error", err)
		return err
	}
	return nil
}

// LogBatchChecked is LogChecked for a WAL replay round: one multi-values
// INSERT (one commit/fsync) instead of one transaction per row. nil means
// EVERY row in entries is durably present — conflict-noop rows count as
// present (EventID unique index), which is exactly the spool's ack
// semantics. A non-nil error means the whole batch must stay in the WAL.
func (s *UsageService) LogBatchChecked(ctx context.Context, entries []*UsageEntry) error {
	if len(entries) == 0 {
		return nil
	}
	logs := make([]*model.UsageLog, len(entries))
	for i, e := range entries {
		logs[i] = buildUsageLog(e)
	}
	if err := s.repo.CreateBatch(ctx, logs); err != nil {
		slog.Error("failed to write usage log batch", "rows", len(logs), "error", err)
		return err
	}
	return nil
}

// firstNonEmpty returns the first argument that is non-empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
