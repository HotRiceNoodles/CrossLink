package guardrail

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

type credentialStreamPattern struct {
	compiledCredentialPattern
	prefix *regexp.Regexp
}

// offsets 将候选文本的每个字节映射回原始字段；URL 转义和 Base64 四元组
// 均保留保守的原始起点，放行水位不会落进编码片段或 UTF-8 字符内部。
type credentialStreamText struct {
	text    string
	offsets []int
}

type credentialStreamField struct {
	bytes   int
	final   bool
	raw     credentialStreamText
	encoded credentialStreamText
	line    credentialBase64Line
}

type credentialStream struct {
	name     string
	patterns []credentialStreamPattern
	fields   map[string]*credentialStreamField
	closed   bool
}

func (e *credentialDetectionEngine) NewStream(ctx context.Context, _ CheckRequest) (StreamSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &credentialStream{name: e.name, fields: map[string]*credentialStreamField{}}
	for _, pattern := range e.activePatterns {
		if pattern.Category == "custom" {
			return nil, ErrStreamUnsupported
		}
		if pattern.Confidence == "low" {
			continue // 沿用低置信度匹配不阻断的约定。
		}
		prefix := credentialStreamPrefixes[pattern.Name]
		if prefix == nil {
			return nil, ErrStreamUnsupported
		}
		s.patterns = append(s.patterns, credentialStreamPattern{pattern, prefix})
	}
	return s, nil
}

func (s *credentialStream) Push(ctx context.Context, f StreamFragment) (StreamVerdict, error) {
	if err := ctx.Err(); err != nil {
		return StreamVerdict{}, err
	}
	if s.closed {
		return StreamVerdict{}, errors.New("credential stream already closed")
	}
	if !utf8.ValidString(f.Text) {
		return StreamVerdict{}, errors.New("invalid UTF-8 credential stream fragment")
	}
	field := s.fields[f.Key]
	if field == nil {
		field = &credentialStreamField{line: credentialBase64Line{start: -1}}
		s.fields[f.Key] = field
	}
	if field.final {
		return StreamVerdict{}, errors.New("credential field already completed")
	}
	start := field.bytes
	field.bytes += len(f.Text)
	verdict := StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: field.bytes}
	if len(s.patterns) == 0 {
		field.final = f.Final
		return verdict, nil
	}
	positions := make([]int, len(f.Text))
	for i := range positions {
		positions[i] = start + i
	}
	var result *GuardrailResult
	var err error
	field.raw, result, err = s.scan(ctx, appendCredentialText(field.raw, credentialStreamText{f.Text, positions}), f.Final)
	if err != nil {
		return StreamVerdict{}, err
	}
	if result != nil {
		verdict.Result = result
		return verdict, nil
	}
	// Base64 必须保持完整行解码的既有语义。只处理新输入，已确认不可能成为
	// Base64 的行立即放弃；候选行不参与明文匹配的重复扫描。
	for i, r := range f.Text {
		if err := ctx.Err(); err != nil {
			return StreamVerdict{}, err
		}
		if r == '\n' {
			result, err = s.finishEncodedLine(ctx, field, start+i+1)
			field.line = credentialBase64Line{start: -1}
			if err != nil {
				return StreamVerdict{}, err
			}
			if result != nil {
				verdict.Result = result
				return verdict, nil
			}
		} else {
			field.line.add(r, start+i)
		}
	}
	if f.Final {
		result, err = s.finishEncodedLine(ctx, field, field.bytes)
		field.line = credentialBase64Line{start: -1}
		if err == nil && result == nil {
			field.encoded, result, err = s.scan(ctx, field.encoded, true)
		}
		if err != nil {
			return StreamVerdict{}, err
		}
		if result != nil {
			verdict.Result = result
			return verdict, nil
		}
	}
	for _, pending := range []credentialStreamText{field.raw, field.encoded} {
		if len(pending.offsets) > 0 && pending.offsets[0] < verdict.ReleaseThrough {
			verdict.ReleaseThrough = pending.offsets[0]
		}
	}
	if !field.line.bad && field.line.start >= 0 && field.line.start < verdict.ReleaseThrough {
		verdict.ReleaseThrough = field.line.start
	}
	field.final = f.Final
	return verdict, ctx.Err()
}

func (s *credentialStream) scan(ctx context.Context, input credentialStreamText, final bool) (credentialStreamText, *GuardrailResult, error) {
	keep, result, err := s.scanText(ctx, input.text, final)
	if err != nil || result != nil {
		return credentialStreamText{}, result, err
	}
	if strings.Contains(input.text, "%") {
		decoded, pending := decodeCredentialStreamURL(input.text, final)
		decodedKeep, hit, err := s.scanText(ctx, decoded.text, final)
		if err != nil || hit != nil {
			return credentialStreamText{}, hit, err
		}
		if decodedKeep < len(decoded.offsets) && decoded.offsets[decodedKeep] < keep {
			keep = decoded.offsets[decodedKeep]
		}
		if pending >= 0 && pending < keep {
			keep = pending
		}
	}
	if final || keep == len(input.text) {
		return credentialStreamText{}, nil, ctx.Err()
	}
	return credentialStreamText{strings.Clone(input.text[keep:]), append([]int(nil), input.offsets[keep:]...)}, nil, ctx.Err()
}

func (s *credentialStream) scanText(ctx context.Context, text string, final bool) (int, *GuardrailResult, error) {
	keep := len(text)
	for _, pattern := range s.patterns {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		for _, loc := range pattern.re.FindAllStringIndex(text, -1) {
			if pattern.Confidence == "medium" {
				// 值末尾尚未确定时，后续仍可能追加 example 等占位符标记。
				if (!final && loc[1] == len(text)) || !validateMediumConfidenceMatch(text[loc[0]:loc[1]]) {
					continue
				}
			}
			return 0, &GuardrailResult{Blocked: true, RuleName: s.name, Severity: pattern.Severity,
				Reason:         "credentials detected: " + pattern.Name,
				ContentSnippet: "[CREDENTIAL:" + pattern.Name + "]"}, nil
		}
		if !final {
			if loc := pattern.prefix.FindStringIndex(text); loc != nil && loc[0] < keep {
				keep = loc[0]
			}
		}
	}
	return keep, nil, ctx.Err()
}

func appendCredentialText(left, right credentialStreamText) credentialStreamText {
	if left.text == "" {
		return right
	}
	positions := make([]int, 0, len(left.offsets)+len(right.offsets))
	positions = append(positions, left.offsets...)
	positions = append(positions, right.offsets...)
	return credentialStreamText{left.text + right.text, positions}
}

func decodeCredentialStreamURL(text string, final bool) (credentialStreamText, int) {
	var out strings.Builder
	positions := make([]int, 0, len(text))
	for i := 0; i < len(text); {
		if text[i] == '%' {
			if !final && (i+1 == len(text) || i+2 == len(text) && credentialHex(text[i+1])) {
				return credentialStreamText{out.String(), positions}, i
			}
			var decoded byte
			if i+2 < len(text) && hexVal(text[i+1], &decoded) && hexVal(text[i+2], &decoded) {
				out.WriteByte(decoded)
				positions = append(positions, i)
				i += 3
				continue
			}
		}
		out.WriteByte(text[i])
		positions = append(positions, i)
		i++
	}
	return credentialStreamText{out.String(), positions}, -1
}

func credentialHex(c byte) bool { var value byte; return hexVal(c, &value) }

type credentialBase64Line struct {
	buffer        strings.Builder
	start         int
	bad, trailing bool
	padded        bool
	standard, url bool
	quartet       []byte
	utf8Tail      []byte
}

func (line *credentialBase64Line) invalidate() {
	*line = credentialBase64Line{start: -1, bad: true}
}

func (line *credentialBase64Line) add(r rune, offset int) {
	if line.bad {
		return
	}
	// Go 的 Base64 解码允许行内 CR；它不结束逻辑行，也不打断四元组。
	if r == '\r' {
		if line.start >= 0 {
			line.buffer.WriteByte('\r')
		}
		return
	}
	if unicode.IsSpace(r) {
		if line.start >= 0 {
			line.trailing = true
			line.buffer.WriteRune(r)
		}
		return
	}
	if line.trailing || line.padded || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("+/-_=", r)) {
		line.invalidate()
		return
	}
	if line.start < 0 {
		line.start = offset
	}
	line.buffer.WriteByte(byte(r))
	line.standard = line.standard || r == '+' || r == '/'
	line.url = line.url || r == '-' || r == '_'
	if line.standard && line.url {
		line.invalidate()
		return
	}
	line.quartet = append(line.quartet, byte(r))
	if len(line.quartet) < 4 {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(string(line.quartet))
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(string(line.quartet))
	}
	if err != nil {
		line.invalidate()
		return
	}
	line.utf8Tail = append(line.utf8Tail, decoded...)
	for len(line.utf8Tail) > 0 && utf8.FullRune(line.utf8Tail) {
		r, n := utf8.DecodeRune(line.utf8Tail)
		if r == utf8.RuneError && n == 1 {
			line.invalidate()
			return
		}
		line.utf8Tail = line.utf8Tail[n:]
	}
	line.padded = line.quartet[3] == '='
	line.quartet = line.quartet[:0]
	if line.padded && len(line.utf8Tail) > 0 {
		line.invalidate()
	}
}

func (s *credentialStream) finishEncodedLine(ctx context.Context, field *credentialStreamField, end int) (*GuardrailResult, error) {
	line := &field.line
	if line.bad || line.start < 0 {
		return nil, nil
	}
	text := strings.TrimSpace(line.buffer.String())
	if len(text) < 40 || !isLikelyBase64(text) {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(text)
	}
	if err != nil || !utf8.Valid(decoded) {
		return nil, nil
	}
	sourcePositions := make([]int, 0, len(text))
	for i := range len(text) {
		if text[i] != '\r' {
			sourcePositions = append(sourcePositions, line.start+i)
		}
	}
	positions := make([]int, len(decoded)+1)
	for i := range decoded {
		positions[i] = sourcePositions[i/3*4]
	}
	positions[len(decoded)] = end
	// 完整解码行之间以换行分隔，与完整检测器一致；行结束也确认令牌边界。
	input := appendCredentialText(field.encoded, credentialStreamText{string(decoded) + "\n", positions})
	var result *GuardrailResult
	field.encoded, result, err = s.scan(ctx, input, false)
	return result, err
}

func (s *credentialStream) Close() { s.closed = true; s.fields = nil }

var _ StreamingEngine = (*credentialDetectionEngine)(nil)
