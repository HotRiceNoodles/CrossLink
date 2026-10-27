package guardrail

import (
	"regexp"
	"regexp/syntax"
)

// 内置凭据表达式的前缀语言只编译一次。任意自定义正则仍使用严格回退。
var credentialStreamPrefixes = func() map[string]*regexp.Regexp {
	compiled := make(map[string]*regexp.Regexp)
	for _, pattern := range builtinCredentialPatterns {
		if pattern.Confidence == "low" {
			continue
		}
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
	alt := func(subs ...*syntax.Regexp) *syntax.Regexp {
		return &syntax.Regexp{Op: syntax.OpAlternate, Sub: subs}
	}
	concat := func(subs ...*syntax.Regexp) *syntax.Regexp {
		return &syntax.Regexp{Op: syntax.OpConcat, Sub: subs}
	}
	switch re.Op {
	case syntax.OpConcat:
		parts := []*syntax.Regexp{empty}
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
	case syntax.OpLiteral:
		parts := []*syntax.Regexp{empty}
		for i := 1; i <= len(re.Rune); i++ {
			parts = append(parts, &syntax.Regexp{Op: syntax.OpLiteral, Flags: re.Flags, Rune: re.Rune[:i]})
		}
		return alt(parts...)
	default:
		return alt(empty, re)
	}
}
