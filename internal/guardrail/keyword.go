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
	checkContent := content
	if !e.caseSensitive {
		checkContent = strings.ToLower(content)
	}

	if e.useRegex {
		for i, re := range e.compiledRegex {
			pattern := e.blocklist[i]
			matched := re.FindString(content)
			if matched == "" {
				continue
			}
			lookupKey := matched
			if !e.caseSensitive {
				lookupKey = strings.ToLower(matched)
			}
			if e.allowlist[lookupKey] {
				continue
			}
			return &GuardrailResult{
				Blocked:  true,
				RuleName: e.name,
				Reason:   fmt.Sprintf("matched keyword pattern: %s", pattern),
				Severity: "medium",
			}, nil
		}
		return &GuardrailResult{Blocked: false}, nil
	}

	for _, word := range e.blocklist {
		checkWord := word
		if !e.caseSensitive {
			checkWord = strings.ToLower(word)
		}
		if e.allowlist[checkWord] {
			continue
		}
		if strings.Contains(checkContent, checkWord) {
			return &GuardrailResult{
				Blocked:  true,
				RuleName: e.name,
				Reason:   fmt.Sprintf("matched keyword: %s", word),
				Severity: "medium",
			}, nil
		}
	}

	return &GuardrailResult{Blocked: false}, nil
}

func init() {
	RegisterEngine("keyword_filter", NewKeywordEngineFromConfig)
}
