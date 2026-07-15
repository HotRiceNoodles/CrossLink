package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/crosslink/internal/model"
)

const (
	maxVariableValueBytes = 4 * 1024  // 4KB per variable value
	maxSystemPromptBytes  = 8 * 1024  // 8KB rendered system prompt
)

// reservedVarPattern matches variable names that look like they carry
// untrusted (end-user) content. Such variables are FORCED untrusted regardless
// of the schema's trusted flag, so they can never be interpolated into the
// system prompt position — a structural defense against prompt injection that
// does not rely on template authors self-assessing correctly.
var reservedVarPattern = regexp.MustCompile(`(?i)user|input|query|prompt|message|text|content`)

// FewShotMessage is a static few-shot example. Content is NOT subject to
// {{var}} interpolation (pure static example, per design).
type FewShotMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// VariableDef is one entry of a template's variables_schema.
type VariableDef struct {
	Name     string `json:"name"`
	Type     string `json:"type"`     // "" | string | number | bool
	Required bool   `json:"required"`
	Default  any    `json:"default"`
	Trusted  bool   `json:"trusted"`
}

// RenderedContext is the output of RenderTemplate: the assembled system prompt
// (with trusted variables interpolated) and the static few-shot examples.
type RenderedContext struct {
	SystemPrompt string          `json:"system_prompt"`
	FewShot      []FewShotMessage `json:"few_shot"`
}

// RenderTemplate interpolates trusted variables into the system prompt and
// enforces the injection-safety hard rules:
//   - missing required (no default) → missing_variable
//   - an untrusted variable (schema trusted:false OR reserved-name-forced)
//     referenced in system_prompt → untrusted_var_in_system
//   - a variable value > 4KB → value_too_large
//   - rendered system_prompt > 8KB → system_too_large
//
// few_shot is passed through verbatim (no interpolation). Variable values are
// interpolated once (a value containing {{x}} is not re-expanded).
func RenderTemplate(tpl *model.PromptTemplate, variables map[string]any) (*RenderedContext, error) {
	defs, err := parseVariableSchema(tpl.VariablesSchema)
	if err != nil {
		return nil, fmt.Errorf("invalid variables_schema: %w", err)
	}

	// Resolve + validate each declared variable, splitting trusted vs untrusted.
	resolved := make(map[string]string, len(defs))
	untrusted := make(map[string]bool)
	for _, d := range defs {
		isTrusted := d.Trusted && !reservedVarPattern.MatchString(d.Name)
		if !isTrusted {
			untrusted[d.Name] = true
		}
		raw, present := variables[d.Name]
		if !present {
			if d.Required && d.Default == nil {
				return nil, fmt.Errorf("missing_variable: %s", d.Name)
			}
			if d.Default != nil {
				raw = d.Default
			} else {
				// Optional, no default, not provided: skip (leave unresolved).
				continue
			}
		}
		val, err := coerceVar(d, raw)
		if err != nil {
			return nil, err
		}
		if len(val) > maxVariableValueBytes {
			return nil, fmt.Errorf("value_too_large: %s", d.Name)
		}
		resolved[d.Name] = val
	}

	// Interpolate. Any {{name}} whose name is untrusted AND present in the
	// system prompt is a hard reject (forces author to move it to user message).
	system, err := interpolateOnce(tpl.SystemPrompt, resolved, untrusted)
	if err != nil {
		return nil, err
	}
	if len(system) > maxSystemPromptBytes {
		return nil, fmt.Errorf("system_too_large: rendered %d bytes > %d", len(system), maxSystemPromptBytes)
	}

	fewShot, err := parseFewShot(tpl.FewShot)
	if err != nil {
		return nil, fmt.Errorf("invalid few_shot: %w", err)
	}

	return &RenderedContext{SystemPrompt: system, FewShot: fewShot}, nil
}

func parseVariableSchema(raw []byte) ([]VariableDef, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var defs []VariableDef
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, err
	}
	return defs, nil
}

func parseFewShot(raw []byte) ([]FewShotMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fs []FewShotMessage
	if err := json.Unmarshal(raw, &fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// coerceVar validates + stringifies a variable value per its declared type.
func coerceVar(d VariableDef, raw any) (string, error) {
	switch d.Type {
	case "", "string":
		s, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("type_mismatch: %s expected string", d.Name)
		}
		return s, nil
	case "number":
		switch v := raw.(type) {
		case float64:
			return fmt.Sprintf("%v", v), nil
		case int:
			return fmt.Sprintf("%d", v), nil
		case int64:
			return fmt.Sprintf("%d", v), nil
		default:
			return "", fmt.Errorf("type_mismatch: %s expected number", d.Name)
		}
	case "bool":
		b, ok := raw.(bool)
		if !ok {
			return "", fmt.Errorf("type_mismatch: %s expected bool", d.Name)
		}
		if b {
			return "true", nil
		}
		return "false", nil
	default:
		// Unknown type: accept as string best-effort.
		return fmt.Sprintf("%v", raw), nil
	}
}

var varToken = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// interpolateOnce replaces {{name}} tokens with resolved values in a SINGLE
// pass (substituted text is never re-scanned, so a value containing {{x}} is
// not re-expanded). Returns untrusted_var_in_system error if a token references
// a declared-untrusted variable that was provided.
func interpolateOnce(s string, resolved map[string]string, untrusted map[string]bool) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	last := 0
	for _, idx := range varToken.FindAllStringSubmatchIndex(s, -1) {
		start, end := idx[0], idx[1]
		nameStart, nameEnd := idx[2], idx[3]
		b.WriteString(s[last:start])
		name := s[nameStart:nameEnd]
		// A reserved-name token (looks like user content) in the system prompt is
		// ALWAYS rejected — declared or not, provided or not. The name itself
		// signals untrusted content that must never land in the system position.
		if reservedVarPattern.MatchString(name) {
			return "", fmt.Errorf("untrusted_var_in_system: %s", name)
		}
		if untrusted[name] {
			if _, provided := resolved[name]; provided {
				return "", fmt.Errorf("untrusted_var_in_system: %s", name)
			}
			// declared-untrusted but not provided → leave token verbatim
			b.WriteString(s[start:end])
		} else if val, ok := resolved[name]; ok {
			b.WriteString(val)
		} else {
			// unknown / unprovided var: leave token verbatim
			b.WriteString(s[start:end])
		}
		last = end
	}
	b.WriteString(s[last:])
	return b.String(), nil
}
