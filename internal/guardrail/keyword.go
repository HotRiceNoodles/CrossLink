package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type KeywordEngine struct {
	name          string
	blocklist     []string
	allowlist     map[string]bool
	useRegex      bool
	caseSensitive bool
	compiledRegex []*regexp.Regexp
}

type keywordConfig struct {
	Blocklist     []string `json:"blocklist"`
	Allowlist     []string `json:"allowlist"`
	UseRegex      bool     `json:"use_regex"`
	CaseSensitive bool     `json:"case_sensitive"`
}

func NewKeywordEngineFromConfig(raw json.RawMessage) (GuardrailEngine, error) {
	var cfg keywordConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid keyword config: %w", err)
	}

	allowSet := make(map[string]bool, len(cfg.Allowlist))
	for _, w := range cfg.Allowlist {
		if !cfg.CaseSensitive {
			allowSet[strings.ToLower(w)] = true
		} else {
			allowSet[w] = true
		}
	}

	var compiled []*regexp.Regexp
	if cfg.UseRegex {
		for _, p := range cfg.Blocklist {
			flags := ""
			if !cfg.CaseSensitive {
				flags = "(?i)"
			}
			re, err := regexp.Compile(flags + p)
			if err != nil {
				return nil, fmt.Errorf("invalid regex %q: %w", p, err)
			}
			compiled = append(compiled, re)
		}
	}

	return &KeywordEngine{
		name:          "keyword_filter",
		blocklist:     cfg.Blocklist,
		allowlist:     allowSet,
		useRegex:      cfg.UseRegex,
		caseSensitive: cfg.CaseSensitive,
		compiledRegex: compiled,
	}, nil
}

func (e *KeywordEngine) Name() string { return e.name }

func (e *KeywordEngine) Check(_ context.Context, content string, _ Direction, _ string) (*GuardrailResult, error) {
	spans, reason := e.matchSpans(content)
	if len(spans) == 0 {
		return &GuardrailResult{Blocked: false}, nil
	}
	return &GuardrailResult{Blocked: true, RuleName: e.name, Reason: reason, Severity: "medium",
		MaskedSpans: spans, MaskedContent: applySpans(content, spans)}, nil
}

// 大小写归一化可能改变 UTF-8 字节长度，映射必须指回未经归一化的原文。
func (e *KeywordEngine) normalizedText(content string) (string, []int) {
	if e.caseSensitive {
		return content, nil
	}
	var text strings.Builder
	positions := make([]int, 0, len(content)+1)
	for offset, r := range content {
		lower := strings.ToLower(string(r))
		text.WriteString(lower)
		for range len(lower) {
			positions = append(positions, offset)
		}
	}
	return text.String(), append(positions, len(content))
}

func (e *KeywordEngine) matchSpans(content string) ([]Span, string) {
	var spans []Span
	var reason string
	if e.useRegex {
		for i, re := range e.compiledRegex {
			for _, loc := range re.FindAllStringIndex(content, -1) {
				if loc[0] == loc[1] {
					continue
				}
				key := content[loc[0]:loc[1]]
				if !e.caseSensitive {
					key = strings.ToLower(key)
				}
				if e.allowlist[key] {
					continue
				}
				spans = append(spans, Span{Start: loc[0], End: loc[1], Replacement: "[REDACTED]"})
				if reason == "" {
					reason = fmt.Sprintf("matched keyword pattern: %s", e.blocklist[i])
				}
			}
		}
	} else {
		text, positions := e.normalizedText(content)
		for _, word := range e.blocklist {
			key := word
			if !e.caseSensitive {
				key = strings.ToLower(key)
			}
			if key == "" || e.allowlist[key] {
				continue
			}
			for cursor := 0; cursor < len(text); {
				index := strings.Index(text[cursor:], key)
				if index < 0 {
					break
				}
				start, end := cursor+index, cursor+index+len(key)
				cursor = start + 1 // 重叠命中也必须覆盖。
				if positions != nil {
					start, end = positions[start], positions[end]
				}
				spans = append(spans, Span{Start: start, End: end, Replacement: "[REDACTED]"})
				if reason == "" {
					reason = fmt.Sprintf("matched keyword: %s", word)
				}
			}
		}
	}
	return composeSpans(spans), reason
}

func init() {
	RegisterEngine("keyword_filter", NewKeywordEngineFromConfig)
}
