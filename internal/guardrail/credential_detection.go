package guardrail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

type credentialPattern struct {
	Name       string
	Category   string
	Regex      string
	Severity   string
	Confidence string // "high", "medium", "low"
}

type credentialDetectionEngine struct {
	name           string
	categories     map[string]bool
	activePatterns []compiledCredentialPattern
	maskChar       string
	maskKeepPrefix int
	maskKeepSuffix int
	maxMatches     int
}

type compiledCredentialPattern struct {
	credentialPattern
	re *regexp.Regexp
}

type credentialConfig struct {
	Categories     []string            `json:"categories"`
	CustomPatterns []customCredPattern `json:"custom_patterns"`
	MaskChar       string              `json:"mask_char"`
	MaskKeepPrefix int                 `json:"mask_keep_prefix"`
	MaskKeepSuffix int                 `json:"mask_keep_suffix"`
	MaxMatches     int                 `json:"max_matches_per_check"`
}

type customCredPattern struct {
	Name       string `json:"name"`
	Regex      string `json:"regex"`
	Severity   string `json:"severity"`
	Confidence string `json:"confidence"` // "high" (default), "medium", "low"
}

var builtinCredentialPatterns = []credentialPattern{
	// API Keys — high confidence
	{Name: "openai_key", Category: "api_key", Regex: `sk-[a-zA-Z0-9]{20,}`, Severity: "critical", Confidence: "high"},
	// OpenAI project-scoped / service-account keys carry a hyphenated prefix
	// the plain sk- pattern cannot match (sk- followed by "proj"/"svcacct"
	// is under the 20-char run before the next hyphen).
	{Name: "openai_project_key", Category: "api_key", Regex: `sk-proj-[a-zA-Z0-9_\-]{20,}`, Severity: "critical", Confidence: "high"},
	{Name: "openai_svcacct_key", Category: "api_key", Regex: `sk-svcacct-[a-zA-Z0-9_\-]{20,}`, Severity: "critical", Confidence: "high"},
	{Name: "sk_bf_key", Category: "api_key", Regex: `sk-bf-[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}`, Severity: "critical", Confidence: "high"},
	{Name: "anthropic_key", Category: "api_key", Regex: `sk-ant-[a-zA-Z0-9\-]{20,}`, Severity: "critical", Confidence: "high"},
	{Name: "stripe_live_key", Category: "api_key", Regex: `sk_live_[a-zA-Z0-9]{20,}`, Severity: "critical", Confidence: "high"},
	{Name: "stripe_test_key", Category: "api_key", Regex: `sk_test_[a-zA-Z0-9]{20,}`, Severity: "high", Confidence: "high"},
	{Name: "github_pat", Category: "api_key", Regex: `ghp_[a-zA-Z0-9]{36}`, Severity: "critical", Confidence: "high"},
	{Name: "github_oauth", Category: "api_key", Regex: `gho_[a-zA-Z0-9]{36}`, Severity: "critical", Confidence: "high"},
	{Name: "slack_bot_token", Category: "api_key", Regex: `xoxb-[0-9]{10,13}-[0-9]{10,13}-[a-zA-Z0-9]{24}`, Severity: "critical", Confidence: "high"},
	{Name: "slack_user_token", Category: "api_key", Regex: `xoxp-[0-9]{10,13}-[0-9]{10,13}-[0-9]{10,13}-[a-zA-Z0-9]{24}`, Severity: "critical", Confidence: "high"},

	// API Keys — medium confidence
	{Name: "bearer_token", Category: "api_key", Regex: `Bearer\s+[a-zA-Z0-9\-._~+/]+={0,2}`, Severity: "high", Confidence: "medium"},
	{Name: "generic_api_key", Category: "api_key", Regex: `(?:api[_-]?key|apikey)\s*[:=]\s*['"]?([a-zA-Z0-9]{32,})['"]?`, Severity: "high", Confidence: "medium"},

	// Cloud Credentials
	{Name: "aws_access_key", Category: "cloud_credential", Regex: `AKIA[A-Z0-9]{16}`, Severity: "critical", Confidence: "high"},
	{Name: "aws_secret_key", Category: "cloud_credential", Regex: `(?:aws_secret|secret_access_key)\s*[:=]\s*['"]?([a-zA-Z0-9/+=]{40})['"]?`, Severity: "critical", Confidence: "high"},
	{Name: "aliyun_ak", Category: "cloud_credential", Regex: `LTAI[a-zA-Z0-9]{12,20}`, Severity: "critical", Confidence: "high"},
	{Name: "gcp_service_account", Category: "cloud_credential", Regex: `"type"\s*:\s*"service_account"`, Severity: "critical", Confidence: "high"},
	{Name: "azure_conn_string", Category: "cloud_credential", Regex: `DefaultEndpointsProtocol=https?;AccountName=[^;]+;AccountKey=[^;]+`, Severity: "critical", Confidence: "high"},

	// Private Keys
	{Name: "private_key", Category: "private_key", Regex: `-----BEGIN\s+(?:RSA\s+|EC\s+|DSA\s+|OPENSSH\s+)?PRIVATE KEY-----`, Severity: "critical", Confidence: "high"},

	// Database Strings
	{Name: "mysql_conn", Category: "database_string", Regex: `mysql:\/\/[^\s'"]+:[^\s'"]+@[^\s'"]+`, Severity: "critical", Confidence: "high"},
	{Name: "postgres_conn", Category: "database_string", Regex: `postgres(?:ql)?:\/\/[^\s'"]+:[^\s'"]+@[^\s'"]+`, Severity: "critical", Confidence: "high"},
	{Name: "mongodb_conn", Category: "database_string", Regex: `mongodb(?:\+srv)?:\/\/[^\s'"]+:[^\s'"]+@[^\s'"]+`, Severity: "critical", Confidence: "high"},
	{Name: "redis_conn", Category: "database_string", Regex: `redis:\/\/:[^\s'"]+@[^\s'"]+`, Severity: "critical", Confidence: "high"},

	// JWT — medium confidence
	{Name: "jwt_token", Category: "jwt", Regex: `eyJ[a-zA-Z0-9_-]+\.eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+`, Severity: "high", Confidence: "medium"},

	// .env — low confidence
	{Name: "env_secret", Category: "env_file", Regex: `(?im)^(?:PASSWORD|SECRET|TOKEN|PRIVATE_KEY|CREDENTIAL)\s*=\s*['"]?([a-zA-Z0-9!@#$%^&*]{8,})['"]?`, Severity: "high", Confidence: "low"},
}

var placeholderValues = []string{"example", "xxx", "changeme", "your_", "placeholder", "todo", "test", "dummy", "sample", "replace_", "insert_"}

func NewCredentialDetectionEngineFromConfig(raw json.RawMessage) (GuardrailEngine, error) {
	var cfg credentialConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("invalid credential_detection config: %w", err)
	}

	cats := make(map[string]bool, len(cfg.Categories))
	for _, c := range cfg.Categories {
		cats[c] = true
	}
	if len(cats) == 0 {
		for _, p := range builtinCredentialPatterns {
			cats[p.Category] = true
		}
	}

	var active []compiledCredentialPattern
	for _, p := range builtinCredentialPatterns {
		if !cats[p.Category] {
			continue
		}
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid regex for %s: %w", p.Name, err)
		}
		active = append(active, compiledCredentialPattern{credentialPattern: p, re: re})
	}

	for _, cp := range cfg.CustomPatterns {
		re, err := regexp.Compile(cp.Regex)
		if err != nil {
			return nil, fmt.Errorf("invalid custom regex for %s: %w", cp.Name, err)
		}
		active = append(active, compiledCredentialPattern{
			credentialPattern: credentialPattern{
				Name: cp.Name, Category: "custom",
				Regex: cp.Regex, Severity: cp.Severity, Confidence: defaultConfidence(cp.Confidence),
			},
			re: re,
		})
	}

	maskChar := cfg.MaskChar
	if maskChar == "" {
		maskChar = "*"
	}
	maxMatches := cfg.MaxMatches
	if maxMatches <= 0 {
		maxMatches = 50
	}
	maskPrefix := cfg.MaskKeepPrefix
	if maskPrefix == 0 {
		maskPrefix = 4
	}
	maskSuffix := cfg.MaskKeepSuffix
	if maskSuffix == 0 {
		maskSuffix = 4
	}

	return &credentialDetectionEngine{
		name:           "credential_detection",
		categories:     cats,
		activePatterns: active,
		maskChar:       maskChar,
		maskKeepPrefix: maskPrefix,
		maskKeepSuffix: maskSuffix,
		maxMatches:     maxMatches,
	}, nil
}

func (e *credentialDetectionEngine) Name() string { return e.name }

func (e *credentialDetectionEngine) Check(_ context.Context, content string, _ Direction, _ string) (*GuardrailResult, error) {
	processed := preprocessCredentialContent(content)

	var reasons []string
	var highestSeverity string
	var hasBlockingMatch bool

	for _, p := range e.activePatterns {
		var loc []int
		for _, candidate := range p.re.FindAllStringIndex(processed, -1) {
			if p.Confidence != "medium" || validateMediumConfidenceMatch(processed[candidate[0]:candidate[1]]) {
				loc = candidate
				break
			}
		}
		if loc == nil {
			continue
		}
		match := processed[loc[0]:loc[1]]

		if severityOrder[p.Severity] > severityOrder[highestSeverity] {
			highestSeverity = p.Severity
		}

		// Low-confidence matches are recorded for masking/logging but do not block.
		if p.Confidence != "low" {
			hasBlockingMatch = true
		}

		masked := maskCredential(match, e.maskKeepPrefix, e.maskKeepSuffix, e.maskChar)
		reasons = append(reasons, fmt.Sprintf("%s: %s", p.Name, masked))

		if len(reasons) >= e.maxMatches {
			break
		}
	}

	if len(reasons) == 0 {
		return &GuardrailResult{Blocked: false}, nil
	}

	// 解码副本仅用于检测；替换始终作用于原文，编码凭据也必须有原文字节范围。
	maskedContent, spans := e.maskOriginalCredentialContent(content, processed)

	return &GuardrailResult{
		Blocked:        hasBlockingMatch,
		RuleName:       e.name,
		Reason:         fmt.Sprintf("credentials detected: %s", strings.Join(reasons, ", ")),
		Severity:       highestSeverity,
		MaskedContent:  maskedContent,
		MaskedSpans:    spans,
		ContentSnippet: truncateCred(reasons[0], 80),
	}, nil
}

func validateMediumConfidenceMatch(match string) bool {
	lower := strings.ToLower(match)
	for _, ph := range placeholderValues {
		if strings.Contains(lower, ph) {
			return false
		}
	}
	// Bearer tokens should have reasonable length after "Bearer "
	if strings.HasPrefix(lower, "bearer ") {
		token := strings.TrimSpace(lower[7:])
		runeCount := utf8.RuneCountInString(token)
		return runeCount >= 20
	}
	// Generic api_key values should be long enough
	runeCount := utf8.RuneCountInString(match)
	return runeCount >= 20
}

// maxDecodedExpansion bounds the decoded-append total: decoded segments are
// appended for DETECTION only, and their combined size is capped at 4x the
// original content (U-22) — a large context of decodable lines must not turn
// into an unbounded secondary buffer per request.
func preprocessCredentialContent(content string) string {
	var parts []string
	// Try base64 decode on segments that look base64-encoded
	if decoded := tryBase64Decode(content); decoded != "" {
		parts = append(parts, content, decoded)
	} else {
		parts = append(parts, content)
	}
	// 原文与 Base64 解码结果分别处理 URL 转义，避免另一个安全 Base64 行
	// 使原文中的 URL 编码凭据失去检查。与增量检查的独立通道保持一致。
	for i, count := 0, len(parts); i < count; i++ {
		if base := parts[i]; strings.Contains(base, "%") {
			if decoded := decodeURL(base); decoded != base {
				parts = append(parts, decoded)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func tryBase64Decode(s string) string {
	// Look for lines that are pure base64 (long enough to be meaningful).
	// U-22: accumulate into a slice + single Join (the old per-line string +=
	// was O(n²) on large contexts) and cap the decoded total at 4x the input.
	capTotal := len(s) * 4
	var parts []string
	total := 0
	for _, line := range splitLines(s) {
		line = strings.TrimSpace(line)
		if len(line) >= 40 && isLikelyBase64(line) {
			var decoded []byte
			var err error
			if decoded, err = base64.StdEncoding.DecodeString(line); err != nil {
				decoded, err = base64.URLEncoding.DecodeString(line)
			}
			if err != nil || !utf8.Valid(decoded) {
				continue
			}
			total += len(decoded) + 1
			if total > capTotal {
				break
			}
			parts = append(parts, string(decoded))
		}
	}
	return strings.Join(parts, "\n")
}

func isLikelyBase64(s string) bool {
	b64chars := 0
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' || c == '-' || c == '_' {
			b64chars++
		}
	}
	return float64(b64chars)/float64(len(s)) > 0.9
}

func decodeURL(s string) string {
	var result strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '%' && i+2 < len(s) {
			var b byte
			if hexVal(s[i+1], &b) && hexVal(s[i+2], &b) {
				result.WriteByte(b)
				i += 3
				continue
			}
		}
		result.WriteByte(s[i])
		i++
	}
	return result.String()
}

func hexVal(c byte, out *byte) bool {
	if c >= '0' && c <= '9' {
		*out = (*out << 4) | (c - '0')
		return true
	}
	if c >= 'a' && c <= 'f' {
		*out = (*out << 4) | (c - 'a' + 10)
		return true
	}
	if c >= 'A' && c <= 'F' {
		*out = (*out << 4) | (c - 'A' + 10)
		return true
	}
	return false
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func maskCredential(value string, keepPrefix, keepSuffix int, maskChar string) string {
	runes := []rune(value)
	length := len(runes)
	if length <= keepPrefix+keepSuffix {
		return strings.Repeat(maskChar, length)
	}
	var b strings.Builder
	for i, r := range runes {
		if i < keepPrefix || i >= length-keepSuffix {
			b.WriteRune(r)
		} else {
			b.WriteString(maskChar)
		}
	}
	return b.String()
}

func truncateCred(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxLen]) + "..."
}

func defaultConfidence(c string) string {
	if c == "high" || c == "medium" || c == "low" {
		return c
	}
	return "high"
}

func init() {
	RegisterEngine("credential_detection", NewCredentialDetectionEngineFromConfig)
}
