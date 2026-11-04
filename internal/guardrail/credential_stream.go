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

// 每个候选字节携带其原文完整区间，编码匹配不会在转义或四元组内部释放。
type credentialStreamText struct {
	text    string
	origins []credentialSourceRange
}

func (t credentialStreamText) sourceRange(start, end int) credentialSourceRange {
	source := credentialSourceRange{-1, -1}
	for _, r := range t.origins[start:end] {
		source = mergeCredentialSource(source, r)
	}
	return source
}

type credentialStreamHit struct {
	span   Span
	result *GuardrailResult
}
type credentialLocalHit struct {
	start, end  int
	replacement string
	pattern     credentialStreamPattern
}
type credentialStreamField struct {
	lastByte     byte
	bytes        int
	final        bool
	raw, encoded credentialStreamText
	line         credentialBase64Line
	ready        []credentialStreamHit
}
type credentialStream struct {
	engine   *credentialDetectionEngine
	patterns []credentialStreamPattern
	fields   map[string]*credentialStreamField
	closed   bool
}

func (e *credentialDetectionEngine) NewStream(ctx context.Context, _ CheckRequest) (StreamSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &credentialStream{engine: e, fields: map[string]*credentialStreamField{}}
	for _, pattern := range e.activePatterns {
		if pattern.Category == "custom" {
			return nil, ErrStreamUnsupported
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
	v := StreamVerdict{CheckedThrough: field.bytes, ReleaseThrough: field.bytes}
	if len(s.patterns) == 0 {
		field.final = f.Final
		return v, nil
	}
	positions := make([]credentialSourceRange, len(f.Text))
	for i := range positions {
		positions[i] = credentialSourceRange{start + i, start + i + 1}
	}
	var hits []credentialStreamHit
	var ready []credentialStreamHit
	var err error
	// 已释放前文不再存储，但 ^ 的匹配语义仍须保留前一个物理行状态。
	input := field.raw
	if input.text == "" && start > 0 && field.lastByte != '\n' {
		input = credentialStreamText{"!", []credentialSourceRange{{start, start}}}
	}
	if len(f.Text) > 0 {
		field.lastByte = f.Text[len(f.Text)-1]
	}
	field.raw, hits, ready, err = s.scan(ctx, appendCredentialText(input, credentialStreamText{f.Text, positions}), f.Final)
	if err != nil {
		return StreamVerdict{}, err
	}
	allHits := hits
	field.ready = append(field.ready, ready...)
	for i, r := range f.Text {
		if err := ctx.Err(); err != nil {
			return StreamVerdict{}, err
		}
		if r == '\n' {
			hits, ready, err = s.finishEncodedLine(ctx, field, start+i+1)
			field.line = credentialBase64Line{start: -1}
			if err != nil {
				return StreamVerdict{}, err
			}
			allHits = append(allHits, hits...)
			field.ready = append(field.ready, ready...)
		} else {
			field.line.add(r, start+i)
		}
	}
	if f.Final {
		hits, ready, err = s.finishEncodedLine(ctx, field, field.bytes)
		field.line = credentialBase64Line{start: -1}
		if err != nil {
			return StreamVerdict{}, err
		}
		allHits = append(allHits, hits...)
		field.ready = append(field.ready, ready...)
		field.encoded, hits, ready, err = s.scan(ctx, field.encoded, true)
		if err != nil {
			return StreamVerdict{}, err
		}
		allHits = append(allHits, hits...)
		field.ready = append(field.ready, ready...)
	}
	for _, pending := range []credentialStreamText{field.raw, field.encoded} {
		if len(pending.origins) > 0 {
			source := pending.sourceRange(0, len(pending.origins))
			if source.start >= 0 && source.start < v.ReleaseThrough {
				v.ReleaseThrough = source.start
			}
		}
	}
	if !field.line.bad && field.line.start >= 0 && field.line.start < v.ReleaseThrough {
		v.ReleaseThrough = field.line.start
	}
	// 其他编码通道尚有候选时，完整替换也不能跨越共同放行边界。
	for changed := true; changed; {
		changed = false
		for _, hit := range field.ready {
			if hit.span.Start < v.ReleaseThrough && hit.span.End > v.ReleaseThrough {
				v.ReleaseThrough = hit.span.Start
				changed = true
			}
		}
	}
	pendingReady := field.ready[:0]
	for _, hit := range field.ready {
		if hit.span.End <= v.ReleaseThrough {
			v.Spans = append(v.Spans, hit.span)
			allHits = append(allHits, hit)
		} else {
			pendingReady = append(pendingReady, hit)
		}
	}
	field.ready = pendingReady
	v.Spans = composeSpans(v.Spans)
	for _, hit := range allHits {
		if hit.result.Blocked {
			if v.BlockStart == nil || hit.span.Start < *v.BlockStart {
				first := hit.span.Start
				v.BlockStart = &first
				v.Result = hit.result
			}
		} else if v.Result == nil && len(v.Spans) > 0 {
			v.Result = hit.result
		}
	}
	field.final = f.Final
	return v, ctx.Err()
}

func (s *credentialStream) scan(ctx context.Context, input credentialStreamText, final bool) (credentialStreamText, []credentialStreamHit, []credentialStreamHit, error) {
	keep, local, err := s.scanText(ctx, input.text, final)
	if err != nil {
		return credentialStreamText{}, nil, nil, err
	}
	if strings.Contains(input.text, "%") {
		decoded, pending := decodeCredentialStreamURL(input.text, final)
		decodedKeep, decodedHits, err := s.scanText(ctx, decoded.text, final && pending < 0)
		if err != nil {
			return credentialStreamText{}, nil, nil, err
		}
		if decodedKeep < len(decoded.origins) && decoded.origins[decodedKeep].start < keep {
			keep = decoded.origins[decodedKeep].start
		}
		if pending >= 0 && pending < keep {
			keep = pending
		}
		for _, hit := range decodedHits {
			source := decoded.sourceRange(hit.start, hit.end)
			if source.start >= 0 {
				hit.start, hit.end = source.start, source.end
				local = append(local, hit)
			}
		}
	}
	// 一个匹配可能与另一个未完成候选相交；必须保留相交匹配的完整原文。
	for changed := true; changed; {
		changed = false
		for _, hit := range local {
			if hit.start < keep && hit.end > keep {
				keep = hit.start
				changed = true
			}
		}
	}
	var hits, ready []credentialStreamHit
	for _, hit := range local {
		source := input.sourceRange(hit.start, hit.end)
		if source.start < 0 {
			continue
		}
		result := &GuardrailResult{Blocked: hit.pattern.Confidence != "low", RuleName: s.engine.name, Severity: hit.pattern.Severity,
			Reason: "credentials detected: " + hit.pattern.Name, ContentSnippet: "[CREDENTIAL:" + hit.pattern.Name + "]"}
		mapped := credentialStreamHit{Span{Start: source.start, End: source.end, Replacement: hit.replacement}, result}
		if result.Blocked || hit.end <= keep {
			hits = append(hits, mapped)
		}
		if hit.end <= keep {
			ready = append(ready, mapped)
		}
	}
	if final || keep == len(input.text) {
		return credentialStreamText{}, hits, ready, ctx.Err()
	}
	return credentialStreamText{strings.Clone(input.text[keep:]), append([]credentialSourceRange(nil), input.origins[keep:]...)}, hits, ready, ctx.Err()
}

func (s *credentialStream) scanText(ctx context.Context, text string, final bool) (int, []credentialLocalHit, error) {
	keep := len(text)
	var hits []credentialLocalHit
	for _, pattern := range s.patterns {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if !final {
			if loc := pattern.prefix.FindStringIndex(text); loc != nil && loc[0] < keep {
				keep = loc[0]
			}
		}
		for _, loc := range pattern.re.FindAllStringIndex(text, -1) {
			match := text[loc[0]:loc[1]]
			if loc[0] == loc[1] {
				continue
			}
			if pattern.Confidence == "medium" {
				if !final && loc[1] == len(text) {
					// 可选结束引号可能已使严格前缀语言失效，确认真实边界前仍保留完整匹配。
					keep = min(keep, loc[0])
					continue
				}
				if !validateMediumConfidenceMatch(match) {
					continue
				}
			}
			hits = append(hits, credentialLocalHit{loc[0], loc[1], maskCredential(match, s.engine.maskKeepPrefix, s.engine.maskKeepSuffix, s.engine.maskChar), pattern})
		}
	}
	return keep, hits, ctx.Err()
}

func appendCredentialText(left, right credentialStreamText) credentialStreamText {
	if left.text == "" {
		return right
	}
	positions := make([]credentialSourceRange, 0, len(left.origins)+len(right.origins))
	positions = append(positions, left.origins...)
	positions = append(positions, right.origins...)
	return credentialStreamText{left.text + right.text, positions}
}
func decodeCredentialStreamURL(text string, final bool) (credentialStreamText, int) {
	var out strings.Builder
	positions := make([]credentialSourceRange, 0, len(text))
	for i := 0; i < len(text); {
		if text[i] == '%' {
			if !final && (i+1 == len(text) || i+2 == len(text) && credentialHex(text[i+1])) {
				return credentialStreamText{out.String(), positions}, i
			}
			var decoded byte
			if i+2 < len(text) && hexVal(text[i+1], &decoded) && hexVal(text[i+2], &decoded) {
				out.WriteByte(decoded)
				positions = append(positions, credentialSourceRange{i, i + 3})
				i += 3
				continue
			}
		}
		out.WriteByte(text[i])
		positions = append(positions, credentialSourceRange{i, i + 1})
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

func (s *credentialStream) finishEncodedLine(ctx context.Context, field *credentialStreamField, end int) ([]credentialStreamHit, []credentialStreamHit, error) {
	line := &field.line
	if line.bad || line.start < 0 {
		return nil, nil, nil
	}
	text := strings.TrimSpace(line.buffer.String())
	if len(text) < 40 || !isLikelyBase64(text) {
		return nil, nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(text)
	}
	if err != nil || !utf8.Valid(decoded) {
		return nil, nil, nil
	}
	sourcePositions := make([]int, 0, len(text))
	for i := range len(text) {
		if text[i] != '\r' {
			sourcePositions = append(sourcePositions, line.start+i)
		}
	}
	positions := make([]credentialSourceRange, 0, len(decoded)+1)
	for i := range decoded {
		quartet := i / 3 * 4
		positions = append(positions, credentialSourceRange{sourcePositions[quartet], sourcePositions[quartet+3] + 1})
	}
	positions = append(positions, credentialSourceRange{end, end})
	input := appendCredentialText(field.encoded, credentialStreamText{string(decoded) + "\n", positions})
	var hits, ready []credentialStreamHit
	field.encoded, hits, ready, err = s.scan(ctx, input, false)
	return hits, ready, err
}
func (s *credentialStream) Close() { s.closed = true; s.fields = nil }

var _ StreamingEngine = (*credentialDetectionEngine)(nil)
