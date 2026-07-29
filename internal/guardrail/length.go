package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
)

type LengthEngine struct {
	name           string
	maxInputChars  int
	maxOutputChars int
}

type lengthConfig struct {
	MaxInputChars  int `json:"max_input_chars"`
	MaxOutputChars int `json:"max_output_chars"`
}

func NewLengthEngineFromConfig(raw json.RawMessage) (GuardrailEngine, error) {
	var cfg lengthConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid content_length config: %w", err)
	}

	if cfg.MaxInputChars == 0 {
		cfg.MaxInputChars = 100000
	}
	if cfg.MaxOutputChars == 0 {
		cfg.MaxOutputChars = 50000
	}

	return &LengthEngine{
		name:           "content_length",
		maxInputChars:  cfg.MaxInputChars,
		maxOutputChars: cfg.MaxOutputChars,
	}, nil
}

func (e *LengthEngine) Name() string { return e.name }

func (e *LengthEngine) Check(_ context.Context, content string, direction Direction, _ string) (*GuardrailResult, error) {
	length := len([]rune(content))

	if (direction == DirectionRequest || direction == DirectionBoth) && e.maxInputChars > 0 && length > e.maxInputChars {
		return &GuardrailResult{
			Blocked:  true,
			RuleName: e.name,
			Reason:   fmt.Sprintf("input length %d exceeds max %d characters", length, e.maxInputChars),
			Severity: "low",
		}, nil
	}
	if (direction == DirectionResponse || direction == DirectionBoth) && e.maxOutputChars > 0 && length > e.maxOutputChars {
		return &GuardrailResult{
			Blocked:  true,
			RuleName: e.name,
			Reason:   fmt.Sprintf("output length %d exceeds max %d characters", length, e.maxOutputChars),
			Severity: "low",
		}, nil
	}

	return &GuardrailResult{Blocked: false}, nil
}

func init() {
	RegisterEngine("content_length", NewLengthEngineFromConfig)
}
