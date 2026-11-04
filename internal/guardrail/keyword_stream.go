package guardrail

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

type keywordStreamField struct {
	tail  string
	bytes int
	final bool
}
type keywordStream struct {
	engine *KeywordEngine
	fields map[string]keywordStreamField
	words  []string
	closed bool
}

func (e *KeywordEngine) NewStream(ctx context.Context, _ CheckRequest) (StreamSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// 任意正则可能依赖无界后文，由共享层复用完整检查。
	if e.useRegex {
		return nil, ErrStreamUnsupported
	}
	s := &keywordStream{engine: e, fields: map[string]keywordStreamField{}}
	for _, word := range e.blocklist {
		if !e.caseSensitive {
			word = strings.ToLower(word)
		}
		if word != "" && !e.allowlist[word] {
			s.words = append(s.words, word)
		}
	}
	return s, nil
}

func (s *keywordStream) Push(ctx context.Context, f StreamFragment) (StreamVerdict, error) {
	if err := ctx.Err(); err != nil {
		return StreamVerdict{}, err
	}
	if s.closed {
		return StreamVerdict{}, errors.New("keyword stream already closed")
	}
	if !utf8.ValidString(f.Text) {
		return StreamVerdict{}, errors.New("invalid UTF-8 keyword stream fragment")
	}
	field := s.fields[f.Key]
	if field.final {
		return StreamVerdict{}, errors.New("keyword field already completed")
	}
	text := field.tail + f.Text
	base := field.bytes - len(field.tail)
	field.bytes += len(f.Text)
	spans, reason := s.engine.matchSpans(text)
	release := len(text)
	if !f.Final {
		normal, positions := s.engine.normalizedText(text)
		for _, word := range s.words {
			for size := 1; size < len(word) && size <= len(normal); size++ {
				start := len(normal) - size
				if !utf8.RuneStart(normal[start]) || !utf8.RuneStart(word[size]) || normal[start:] != word[:size] {
					continue
				}
				if positions != nil {
					start = positions[start]
				}
				if start < release {
					release = start
				}
			}
		}
	}
	// 完整命中与候选重叠时，两者一起暂留，避免将来替换追溯已释放范围。
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Start < release && spans[i].End > release {
			release = spans[i].Start
		}
	}
	v := StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: base + release}
	if len(spans) > 0 {
		start := base + spans[0].Start
		v.BlockStart = &start
		v.Result = &GuardrailResult{Blocked: true, RuleName: s.engine.name, Reason: reason, Severity: "medium"}
		for _, sp := range spans {
			if sp.End > release {
				continue
			}
			sp.Start += base
			sp.End += base
			v.Spans = append(v.Spans, sp)
		}
	}
	field.tail = strings.Clone(text[release:])
	field.final = f.Final
	s.fields[f.Key] = field
	return v, nil
}
func (s *keywordStream) Close() { s.closed = true; s.fields = nil; s.words = nil }
