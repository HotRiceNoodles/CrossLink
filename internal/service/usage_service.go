package service

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/crosslink/internal/model"
	"github.com/crosslink/internal/repository"
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
			ServiceUnavailable: true,
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
	UserMessage    string
	ModelResponse  string
	FallbackCount       int
	RetryCount          int
	GuardrailTriggered  bool
	GuardrailRule       string
	AgentType           string
	SecurityEvents      datatypes.JSON
	CacheHit            bool
	ReasoningTokens     int
	CacheReadTokens     int
	SessionID           string
	TemplateID      *int64 // prompt template that assembled this request (nil = none)
	PrecomputedCost float64
}

func (e *UsageEntry) cost() float64 {
	if e.PrecomputedCost > 0 {
		return e.PrecomputedCost
	}
	return e.InputPrice*float64(e.InputTokens)/1000 + e.OutputPrice*float64(e.OutputTokens)/1000
}

func (s *UsageService) Log(ctx context.Context, entry *UsageEntry) {
	log := &model.UsageLog{
		RequestID:      uuid.New().String()[:8],
		RouteType:      entry.RouteType,
		ModelRequested: entry.ModelRequested,
		ModelUsed:      entry.ModelUsed,
		InputTokens:    entry.InputTokens,
		OutputTokens:   entry.OutputTokens,
		Cost:           entry.cost(),
		Currency:       model.ValidCurrency(entry.Currency),
		LatencyMs:      int(entry.LatencyMs),
		StatusCode:     entry.StatusCode,
		ErrorType:     entry.ErrorType,
		FallbackCount:      entry.FallbackCount,
		RetryCount:         entry.RetryCount,
		GuardrailTriggered: entry.GuardrailTriggered,
		GuardrailRule:      entry.GuardrailRule,
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

	if err := s.repo.Create(ctx, log); err != nil {
		slog.Error("failed to write usage log", "error", err)
	}
}
