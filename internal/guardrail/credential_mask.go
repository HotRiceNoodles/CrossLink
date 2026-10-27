package guardrail

import (
	"encoding/base64"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 每个解码字节对应原文中的完整区间；Base64 字节保守覆盖整个四元组。
type credentialSourceRange struct{ start, end int }

type credentialMaskPart struct {
	text    string
	origins []credentialSourceRange // nil 表示原文，字节位置保持不变。
}

func mergeCredentialSource(left, right credentialSourceRange) credentialSourceRange {
	if right.start < 0 || right.end <= right.start {
		return left
	}
	if left.start < 0 {
		return right
	}
	return credentialSourceRange{min(left.start, right.start), max(left.end, right.end)}
}

func (p credentialMaskPart) sourceRange(start, end int) credentialSourceRange {
	if p.origins == nil {
		return credentialSourceRange{start, end}
	}
	span := credentialSourceRange{-1, -1}
	for _, origin := range p.origins[start:end] {
		span = mergeCredentialSource(span, origin)
	}
	return span
}

// 仅在已命中且存在解码副本时建立映射，正常未命中路径不分配原文映射数组。
// 副本顺序和分隔符严格沿用 preprocessCredentialContent 的检测语义。
func credentialMaskParts(content string) []credentialMaskPart {
	parts := []credentialMaskPart{{text: content}}
	if decoded := credentialBase64MaskPart(content); decoded.text != "" {
		parts = append(parts, decoded)
	}
	for i, count := 0, len(parts); i < count; i++ {
		if strings.Contains(parts[i].text, "%") {
			if decoded := credentialURLMaskPart(parts[i]); decoded.text != parts[i].text {
				parts = append(parts, decoded)
			}
		}
	}
	return parts
}

func credentialURLMaskPart(input credentialMaskPart) credentialMaskPart {
	var out strings.Builder
	origins := make([]credentialSourceRange, 0, len(input.text))
	for i := 0; i < len(input.text); {
		var decoded byte
		if input.text[i] == '%' && i+2 < len(input.text) && hexVal(input.text[i+1], &decoded) && hexVal(input.text[i+2], &decoded) {
			out.WriteByte(decoded)
			origins = append(origins, input.sourceRange(i, i+3))
			i += 3
		} else {
			out.WriteByte(input.text[i])
			origins = append(origins, input.sourceRange(i, i+1))
			i++
		}
	}
	return credentialMaskPart{out.String(), origins}
}

func credentialBase64MaskPart(content string) credentialMaskPart {
	var out strings.Builder
	var origins []credentialSourceRange
	total, offset := 0, 0
	for _, line := range splitLines(content) {
		start := offset
		offset += len(line) + 1
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < 40 || !isLikelyBase64(trimmed) {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(trimmed)
		if err != nil {
			decoded, err = base64.URLEncoding.DecodeString(trimmed)
		}
		if err != nil || !utf8.Valid(decoded) {
			continue
		}
		total += len(decoded) + 1
		if total > len(content)*4 {
			break
		}
		start += len(line) - len(strings.TrimLeftFunc(line, unicode.IsSpace))
		positions := make([]int, 0, len(trimmed))
		for i := range len(trimmed) {
			if trimmed[i] != '\r' {
				positions = append(positions, start+i)
			}
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
			origins = append(origins, credentialSourceRange{-1, -1})
		}
		out.Write(decoded)
		for i := range len(decoded) {
			quartet := i / 3 * 4
			origins = append(origins, credentialSourceRange{positions[quartet], positions[quartet+3] + 1})
		}
	}
	return credentialMaskPart{out.String(), origins}
}

func (e *credentialDetectionEngine) maskOriginalCredentialContent(content, processed string) (string, []Span) {
	parts := []credentialMaskPart{{text: content}}
	if processed != content {
		parts = credentialMaskParts(content)
	}
	var spans []Span
	for _, pattern := range e.activePatterns {
		for _, loc := range pattern.re.FindAllStringIndex(processed, -1) {
			match := processed[loc[0]:loc[1]]
			if pattern.Confidence == "medium" && !validateMediumConfidenceMatch(match) {
				continue
			}
			source := credentialSourceRange{-1, -1}
			cursor := 0
			for _, part := range parts {
				start, end := max(loc[0]-cursor, 0), min(loc[1]-cursor, len(part.text))
				if start < end {
					source = mergeCredentialSource(source, part.sourceRange(start, end))
				}
				cursor += len(part.text) + 1
			}
			if source.start >= 0 && source.end <= len(content) {
				spans = append(spans, Span{Start: source.start, End: source.end,
					Replacement: maskCredential(match, e.maskKeepPrefix, e.maskKeepSuffix, e.maskChar)})
			}
		}
	}
	spans = composeSpans(spans)
	if len(spans) == 0 && content != "" {
		// 任意自定义正则若仅命中合成分隔符，也不能声称完成脱敏后放行原文。
		spans = []Span{{Start: 0, End: len(content), Replacement: "[REDACTED:CREDENTIAL]"}}
	}
	return applySpans(content, spans), spans
}
