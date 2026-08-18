package handler

import (
	"encoding/json"
	"log/slog"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/service"
	"gorm.io/datatypes"
)

// contextAnalysisInput carries everything the async analysis needs.
// Built SYNCHRONOUSLY in the handler — gin.Context must not be touched
// inside the async submitUsage closure (project rule, see template_id.go).
type contextAnalysisInput struct {
	anthropicReq   *domain.AnthropicRequest
	openaiReq      *domain.OpenAIRequest
	maxContext     *int  // from primary route at request time (nil = unknown)
	maxTokens      int
	modelUsed      string // actual serving model (fallback-aware)
	observeFn      func(model string, actual, estimated int) // nil = no calibration
	inputFromUpstr bool   // true only when inputTokens is real upstream usage
	inputTokens    int
}

// contextAnalysisResult is what lands in UsageEntry. Nil fields = unanalyzed.
type contextAnalysisResult struct {
	SystemTokens, HistoryTokens, QuestionTokens        *int
	ToolTokens, ToolOutputTokens                       *int
	ContextWindow, ContextUtilizationBp, AnalysisFlags *int
	Snapshot                                           datatypes.JSON
}

// applyContextAnalysis writes the nine analysis fields onto a UsageEntry.
// No-op when analysis failed (nil) — entry keeps its nil = unanalyzed state.
func (r *contextAnalysisResult) apply(entry *service.UsageEntry) {
	if r == nil {
		return
	}
	entry.SystemTokens = r.SystemTokens
	entry.HistoryTokens = r.HistoryTokens
	entry.QuestionTokens = r.QuestionTokens
	entry.ToolTokens = r.ToolTokens
	entry.ToolOutputTokens = r.ToolOutputTokens
	entry.ContextWindow = r.ContextWindow
	entry.ContextUtilizationBp = r.ContextUtilizationBp
	entry.AnalysisFlags = r.AnalysisFlags
	entry.ContextSnapshot = r.Snapshot
}

// BuildContextAnalysisResult runs the five-bucket context analysis. Returns
// nil on any failure (caller leaves UsageEntry fields nil = unanalyzed).
// Never panics outward. Calibration only runs on real upstream usage.
func BuildContextAnalysisResult(in contextAnalysisInput) (res *contextAnalysisResult) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("context analysis panic recovered", "panic", r)
		}
	}()

	var buckets service.ContextBuckets
	switch {
	case in.anthropicReq != nil:
		buckets = service.AnalyzeAnthropicBuckets(in.anthropicReq)
	case in.openaiReq != nil:
		buckets = service.AnalyzeOpenAIBuckets(in.openaiReq)
	default:
		return nil
	}

	window := 0
	if in.maxContext != nil {
		window = *in.maxContext
	}
	if window <= 0 && in.modelUsed != "" {
		if w := service.DefaultContextWindow(in.modelUsed); w > 0 {
			window = w
		}
	}

	flags, bp := service.ComputeAnalysisFlags(buckets, window, in.maxTokens)

	var snap struct {
		Buckets service.ContextBuckets `json:"buckets"`
	}
	snap.Buckets = buckets
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil
	}

	i := func(v int) *int { return &v }
	windowPtr := func() *int {
		if window <= 0 {
			return nil
		}
		return &window
	}
	bpPtr := func() *int {
		if bp < 0 {
			return nil
		}
		return &bp
	}
	res = &contextAnalysisResult{
		SystemTokens:         i(buckets.SystemTokens),
		HistoryTokens:        i(buckets.HistoryTokens),
		QuestionTokens:       i(buckets.QuestionTokens),
		ToolTokens:           i(buckets.ToolTokens),
		ToolOutputTokens:     i(buckets.ToolOutputTokens),
		ContextWindow:        windowPtr(),
		ContextUtilizationBp: bpPtr(),
		AnalysisFlags:        i(flags),
		Snapshot:             datatypes.JSON(raw),
	}

	// Calibration observe runs last. The deferred recover does not assign
	// res, so the outcome depends on where the panic happens: a panic in
	// the bucket analysis above leaves res nil (no result); a panic in the
	// observe hook below preserves the already-computed res.
	if in.observeFn != nil && in.inputFromUpstr && in.inputTokens > 0 {
		in.observeFn(in.modelUsed, in.inputTokens, buckets.Total())
	}
	return res
}

// calibrationObserveOf adapts the calibration service into a plain observe
// function for contextAnalysisInput. Returns nil when calibration is not
// wired (Task 9), which disables observing without any nil checks at callsites.
func calibrationObserveOf(c *service.CalibrationService) func(string, int, int) {
	if c == nil {
		return nil
	}
	return c.Observe
}
