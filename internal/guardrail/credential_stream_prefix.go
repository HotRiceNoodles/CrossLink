package guardrail

import (
	"regexp"
	"regexp/syntax"
)

// 内置表达式的严格前缀语言不包含最后一个已确定字符；可变长匹配仍保留
// 到真实边界，固定长度匹配完成后则立即生成完整替换区间。
var credentialStreamPrefixes = func() map[string]*regexp.Regexp {
	compiled := make(map[string]*regexp.Regexp)
	for _, pattern := range builtinCredentialPatterns {
		re, err := syntax.Parse(pattern.Regex, syntax.Perl)
		if err != nil {
			panic(err)
		}
		compiled[pattern.Name] = regexp.MustCompile("(?:" + credentialPrefix(re.Simplify()).String() + `)\z`)
	}
	return compiled
}()

func credentialPrefix(re *syntax.Regexp) *syntax.Regexp {
	empty := &syntax.Regexp{Op: syntax.OpEmptyMatch}
	none := &syntax.Regexp{Op: syntax.OpNoMatch}
	alt := func(subs ...*syntax.Regexp) *syntax.Regexp { return &syntax.Regexp{Op: syntax.OpAlternate, Sub: subs} }
	concat := func(subs ...*syntax.Regexp) *syntax.Regexp { return &syntax.Regexp{Op: syntax.OpConcat, Sub: subs} }
	switch re.Op {
	case syntax.OpConcat:
		parts := []*syntax.Regexp{}
		for i, sub := range re.Sub {
			path := append([]*syntax.Regexp{}, re.Sub[:i]...)
			parts = append(parts, concat(append(path, credentialPrefix(sub))...))
		}
		return alt(parts...)
	case syntax.OpAlternate:
		parts := make([]*syntax.Regexp, 0, len(re.Sub))
		for _, sub := range re.Sub {
			parts = append(parts, credentialPrefix(sub))
		}
		return alt(parts...)
	case syntax.OpCapture, syntax.OpQuest:
		return credentialPrefix(re.Sub[0])
	case syntax.OpStar, syntax.OpPlus:
		return concat(&syntax.Regexp{Op: syntax.OpStar, Sub: re.Sub}, credentialPrefix(re.Sub[0]))
	case syntax.OpRepeat:
		maximum := re.Max
		if maximum == 0 {
			return none
		}
		if maximum > 0 {
			maximum--
		}
		return concat(&syntax.Regexp{Op: syntax.OpRepeat, Sub: re.Sub, Min: 0, Max: maximum}, credentialPrefix(re.Sub[0]))
	case syntax.OpLiteral:
		parts := []*syntax.Regexp{empty}
		for i := 1; i < len(re.Rune); i++ {
			parts = append(parts, &syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: re.Rune[:i]})
		}
		return alt(parts...)
	case syntax.OpCharClass, syntax.OpAnyCharNotNL, syntax.OpAnyChar:
		return empty
	default:
		return none
	}
}
