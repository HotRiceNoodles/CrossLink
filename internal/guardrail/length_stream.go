package guardrail

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

type lengthStreamField struct {
	bytes int
	runes int
	final bool
	tool  bool
}

type lengthStream struct {
	engine *LengthEngine
	fields map[string]lengthStreamField
	runes  int
	closed bool
}

func (e *LengthEngine) NewStream(ctx context.Context, _ CheckRequest) (StreamSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &lengthStream{engine: e, fields: map[string]lengthStreamField{}}, nil
}

func (s *lengthStream) Push(ctx context.Context, f StreamFragment) (StreamVerdict, error) {
	if err := ctx.Err(); err != nil {
		return StreamVerdict{}, err
	}
	if s.closed {
		return StreamVerdict{}, errors.New("length stream already closed")
	}
	if !utf8.ValidString(f.Text) {
		return StreamVerdict{}, errors.New("invalid UTF-8 length stream fragment")
	}
	field, exists := s.fields[f.Key]
	if field.final {
		return StreamVerdict{}, errors.New("length field already completed")
	}
	// 与完整响应的字段拼接口径一致：各字段之间计一个换行符。
	if !exists && len(s.fields) > 0 {
		s.runes++
	}
	if !exists {
		field.tool = f.Tool
	}
	before := s.runes
	previousBytes := field.bytes
	added := utf8.RuneCountInString(f.Text)
	if field.tool {
		added = 0 // 完整 JSON 的规范文本由 CheckReplacement 一次性计入。
	}
	s.runes += added
	field.runes += added
	field.bytes += len(f.Text)
	field.final = f.Final
	s.fields[f.Key] = field
	if result := s.check(); result != nil {
		// 原文计数达到 N 仍合法；只截断第 N+1 个字符及其后文。
		remaining := max(s.engine.maxOutputChars-before, 0)
		start := previousBytes
		if !field.tool {
			for offset := range f.Text {
				if remaining == 0 {
					break
				}
				_, size := utf8.DecodeRuneInString(f.Text[offset:])
				start = previousBytes + offset + size
				remaining--
			}
		}
		return StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: start, BlockStart: &start, Result: result}, nil
	}
	return StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: field.bytes}, nil
}

func (s *lengthStream) CheckReplacement(ctx context.Context, key, text string) (*GuardrailResult, error) {
	return s.CheckReplacements(ctx, map[string]string{key: text})
}

func (s *lengthStream) CheckReplacements(ctx context.Context, texts map[string]string) (*GuardrailResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, errors.New("length stream already closed")
	}
	delta := 0
	for key, text := range texts {
		field, exists := s.fields[key]
		if !exists || !utf8.ValidString(text) {
			return nil, errors.New("invalid length snapshot replacement")
		}
		delta += utf8.RuneCountInString(text) - field.runes
	}
	for key, text := range texts {
		field := s.fields[key]
		field.runes = utf8.RuneCountInString(text)
		s.fields[key] = field
	}
	s.runes += delta
	return s.check(), nil
}

func (s *lengthStream) check() *GuardrailResult {
	if limit := s.engine.maxOutputChars; limit > 0 && s.runes > limit {
		return &GuardrailResult{
			Blocked: true, RuleName: s.engine.name, Severity: "low",
			Reason:         fmt.Sprintf("output length %d exceeds max %d characters", s.runes, limit),
			ContentSnippet: "[OUTPUT_LENGTH_EXCEEDED]",
		}
	}
	return nil
}

func (s *lengthStream) Close() { s.closed = true; s.fields = nil }

var _ StreamingEngine = (*LengthEngine)(nil)
