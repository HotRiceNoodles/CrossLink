package guardrail

import (
	"context"
	"strings"
)

type keywordStreamField struct {
	tail  string
	bytes int
}
type keywordStream struct {
	engine *KeywordEngine
	fields map[string]keywordStreamField
	words  []string
}

func (e *KeywordEngine) NewStream(_ context.Context, _ CheckRequest) (StreamSession, error) {
	// Arbitrary regexes may need unbounded lookahead. Their existing strict
	// checker remains authoritative instead of guessing an overlap size.
	if e.useRegex {
		return nil, ErrStreamUnsupported
	}
	s := &keywordStream{engine: e, fields: map[string]keywordStreamField{}}
	for _, word := range e.blocklist {
		if !e.caseSensitive {
			word = strings.ToLower(word)
		}
		if !e.allowlist[word] {
			s.words = append(s.words, word)
		}
	}
	return s, nil
}

func (s *keywordStream) Push(ctx context.Context, f StreamFragment) (StreamVerdict, error) {
	if err := ctx.Err(); err != nil {
		return StreamVerdict{}, err
	}
	field := s.fields[f.Key]
	text := field.tail + f.Text
	field.bytes += len(f.Text)
	result, _ := s.engine.Check(ctx, text, DirectionResponse, "")
	if result.Blocked {
		return StreamVerdict{CheckedThrough: field.bytes, Result: result}, nil
	}
	// Retain exactly the longest suffix that can still become a blocked
	// literal. Both the candidate and original use rune indexes so case
	// folding cannot corrupt UTF-8 byte offsets (e.g. Kelvin sign -> k).
	normal := text
	if !s.engine.caseSensitive {
		normal = strings.ToLower(normal)
	}
	chars := []rune(normal)
	original := []rune(text)
	keep := 0
	if !f.Final {
		for _, word := range s.words {
			pattern := []rune(word)
			for n := 1; n < len(pattern) && n <= len(chars); n++ {
				if n > keep && string(chars[len(chars)-n:]) == string(pattern[:n]) {
					keep = n
				}
			}
		}
	}
	field.tail = string(original[len(original)-keep:])
	s.fields[f.Key] = field
	return StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: field.bytes - len(field.tail)}, nil
}
func (s *keywordStream) Close() { s.fields = nil; s.words = nil }
