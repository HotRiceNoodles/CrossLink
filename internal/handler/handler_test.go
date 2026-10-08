package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/provider"
	"github.com/crosslink/internal/router"
	"github.com/crosslink/internal/service"
	"github.com/crosslink/internal/translator"
)

func TestTruncateContent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"short string", "hello", "hello"},
		{"empty string", "", ""},
		{"at limit", string(make([]rune, maxContentLen)), string(make([]rune, maxContentLen))},
		{"over limit by one", string(make([]rune, maxContentLen+1)), string(make([]rune, maxContentLen))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateContent(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("truncateContent() len = %d, want %d", len(got), len(tt.want))
			}
		})
	}

	// Multi-byte: rune truncation, not byte truncation
	cjk := "你好世界"
	long := ""
	for i := 0; i < maxContentLen+100; i++ {
		long += cjk
	}
	got := truncateContent(long)
	if runeCount := len([]rune(got)); runeCount != maxContentLen {
		t.Errorf("truncateContent multi-byte rune count = %d, want %d", runeCount, maxContentLen)
	}
}

func TestExtractLastUserMessage(t *testing.T) {
	contentText := func(s string) json.RawMessage {
		b, _ := json.Marshal([]domain.ContentBlock{{Type: "text", Text: s}})
		return b
	}

	tests := []struct {
		name     string
		messages []domain.AnthropicMessage
		want     string
	}{
		{"empty", nil, ""},
		{"single user", []domain.AnthropicMessage{{Role: "user", Content: contentText("hi")}}, "hi"},
		{"mixed roles", []domain.AnthropicMessage{
			{Role: "user", Content: contentText("first")},
			{Role: "assistant", Content: contentText("response")},
			{Role: "user", Content: contentText("second")},
		}, "second"},
		{"no user", []domain.AnthropicMessage{
			{Role: "assistant", Content: contentText("hello")},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLastUserMessage(tt.messages)
			if got != tt.want {
				t.Errorf("extractLastUserMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractLastOpenAIUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		messages []domain.OpenAIMessage
		want     string
	}{
		{"empty", nil, ""},
		{"single user", []domain.OpenAIMessage{{Role: "user", Content: "hi"}}, "hi"},
		{"mixed roles", []domain.OpenAIMessage{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "response"},
			{Role: "user", Content: "second"},
		}, "second"},
		{"no user", []domain.OpenAIMessage{
			{Role: "assistant", Content: "hello"},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLastOpenAIUserMessage(tt.messages)
			if got != tt.want {
				t.Errorf("extractLastOpenAIUserMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMapProviderErrorStatus(t *testing.T) {	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil error", nil, 502},
		{"429 rate limit", &provider.ProviderError{StatusCode: 429}, 429},
		{"400 bad request", &provider.ProviderError{StatusCode: 400}, 400},
		{"401 auth", &provider.ProviderError{StatusCode: 401}, 502},
		{"500 server", &provider.ProviderError{StatusCode: 500}, 502},
		{"generic error", errors.New("something"), 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapProviderErrorStatus(tt.err)
			if got != tt.want {
				t.Errorf("mapProviderErrorStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

// gatewayErrorStatus replaces the logFailure-hardcoded 502: the usage log must
// record the same class the client saw (upstream 400 → 400, not 502).
func TestGatewayErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"upstream 400", &provider.ProviderError{StatusCode: 400}, 400},
		{"upstream 429", &provider.ProviderError{StatusCode: 429}, 429},
		{"upstream 401 masked as 502", &provider.ProviderError{StatusCode: 401}, 502},
		{"upstream 500 stays 500", &provider.ProviderError{StatusCode: 500}, 500},
		{"translator missing model", translator.ErrMissingModel, 400},
		{"generic error", errors.New("boom"), 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := gatewayErrorStatus(tt.err); got != tt.want {
				t.Errorf("gatewayErrorStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestErrorInfo(t *testing.T) {
	t.Run("provider error carries upstream fields and stage", func(t *testing.T) {
		pe := &provider.ProviderError{
			StatusCode: 400, Message: "provider bad request: Invalid value org-abc123456",
			Code: "model_not_found", Type: "invalid_request_error", Param: "max_tokens",
		}
		d := errorInfo(pe)
		if d.Message != "provider bad request: Invalid value [REDACTED]" {
			t.Errorf("Message = %q, want redacted org ID", d.Message)
		}
		if d.UpstreamStatus != 400 || d.Code != "model_not_found" || d.Type != "invalid_request_error" || d.Param != "max_tokens" {
			t.Errorf("upstream fields = %+v", d)
		}
		if d.Stage != "upstream" {
			t.Errorf("Stage = %q, want upstream", d.Stage)
		}
	})
	t.Run("gateway-side rejection infers translate stage, no upstream fields", func(t *testing.T) {
		d := errorInfo(translator.ErrMissingModel)
		if d.Stage != "translate" {
			t.Errorf("Stage = %q, want translate", d.Stage)
		}
		if d.UpstreamStatus != 0 || d.Code != "" || d.Param != "" {
			t.Errorf("upstream fields should be empty, got %+v", d)
		}
	})
	t.Run("unknown error falls to internal stage", func(t *testing.T) {
		if d := errorInfo(errors.New("boom")); d.Stage != "internal" {
			t.Errorf("Stage = %q, want internal", d.Stage)
		}
	})
}

func TestSanitizeMessageRuneSafe(t *testing.T) {
	// 300 CJK runes: byte-length 900 > 200, rune-length 300 > 200. A byte
	// slice would cut the last character in half and leave invalid UTF-8.
	long := strings.Repeat("错", 300)
	got := sanitizeProviderMessage(long)
	if !utf8.ValidString(got) {
		t.Fatalf("client copy is not valid UTF-8: %q", got)
	}
	if r := len([]rune(got)); r > 200 {
		t.Errorf("client copy = %d runes, want <= 200", r)
	}
	// Storage copy keeps more of the original.
	gotLog := sanitizeMessageForLog(long)
	if r := len([]rune(gotLog)); r != 300 {
		t.Errorf("storage copy = %d runes, want 300 (under limit)", r)
	}
}

func TestAttemptsJSON(t *testing.T) {
	t.Run("clean single-attempt success stays nil", func(t *testing.T) {
		if got := attemptsJSON([]service.FallbackAttempt{{ProviderName: "p", ProviderModel: "m", Success: true}}); got != nil {
			t.Errorf("attemptsJSON() = %s, want nil", got)
		}
	})
	t.Run("empty stays nil", func(t *testing.T) {
		if got := attemptsJSON(nil); got != nil {
			t.Errorf("attemptsJSON(nil) = %s, want nil", got)
		}
	})
	t.Run("timeline serializes provider/model/status", func(t *testing.T) {
		got := attemptsJSON([]service.FallbackAttempt{
			{ProviderName: "openai", ProviderModel: "gpt-4o", ErrorType: "bad_request", UpstreamStatus: 400, LatencyMs: 120, Success: false},
			{ProviderName: "azure", ProviderModel: "gpt-4o", LatencyMs: 300, Success: true},
		})
		if got == nil {
			t.Fatal("attemptsJSON() = nil, want timeline")
		}
		var parsed []attemptRecord
		if err := json.Unmarshal(got, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(parsed) != 2 {
			t.Fatalf("len = %d, want 2", len(parsed))
		}
		if parsed[0].Provider != "openai" || parsed[0].Model != "gpt-4o" || parsed[0].UpstreamStatus != 400 || parsed[0].ErrorType != "bad_request" {
			t.Errorf("first attempt = %+v", parsed[0])
		}
		if !parsed[1].Success || parsed[1].ErrorType != "" {
			t.Errorf("second attempt = %+v", parsed[1])
		}
	})
}

// Upstream 429 must surface as rate_limit_error with Retry-After so clients
// back off correctly instead of treating it as a generic API failure.
func TestAnthropicWriteError_UpstreamRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func() (*gin.Context, *httptest.ResponseRecorder) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		return c, w
	}

	t.Run("429 with Retry-After hint", func(t *testing.T) {
		c, w := newCtx()
		h := &AnthropicHandler{}
		h.writeError(c, &provider.ProviderError{StatusCode: 429, RetryAfter: 12 * time.Second}, "m")
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra != "12" {
			t.Errorf("Retry-After = %q, want %q", ra, "12")
		}
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Error.Type != "rate_limit_error" {
			t.Errorf("error type = %q, want rate_limit_error", body.Error.Type)
		}
	})

	t.Run("429 without hint keeps no Retry-After", func(t *testing.T) {
		c, w := newCtx()
		h := &AnthropicHandler{}
		h.writeError(c, &provider.ProviderError{StatusCode: 429}, "m")
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("status = %d, want 429", w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra != "" {
			t.Errorf("Retry-After = %q, want empty", ra)
		}
	})

	t.Run("non-429 stays api_error", func(t *testing.T) {
		c, w := newCtx()
		h := &AnthropicHandler{}
		h.writeError(c, &provider.ProviderError{StatusCode: 500, RetryAfter: 30 * time.Second}, "m")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
		if ra := w.Header().Get("Retry-After"); ra != "" {
			t.Errorf("Retry-After = %q, want empty", ra)
		}
		var body struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if body.Error.Type != "api_error" {
			t.Errorf("error type = %q, want api_error", body.Error.Type)
		}
	})
}

func TestResolveErrorStatus(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"alias on Community (Pro required)", router.ErrProRequired, http.StatusForbidden},
		{"wrapped Pro required", fmt.Errorf("resolve route: %w", router.ErrProRequired), http.StatusForbidden},
		{"generic resolve error", errors.New("no active provider"), http.StatusNotFound},
		{"nil error", nil, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveErrorStatus(tt.err)
			if got != tt.want {
				t.Errorf("resolveErrorStatus() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSafeProviderError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"provider error", &provider.ProviderError{Message: "rate limited"}, "rate limited"},
		{"generic error", errors.New("internal"), "upstream provider error"},
		{"nil error", nil, "upstream provider error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := safeProviderError(tt.err)
			if got != tt.want {
				t.Errorf("safeProviderError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFirstTokenMsValue(t *testing.T) {
	start := time.Now()

	tests := []struct {
		name         string
		start        time.Time
		firstTokenAt time.Time
		want         int64
	}{
		{"zero firstTokenAt", start, time.Time{}, 0},
		{"valid diff", start, start.Add(150 * time.Millisecond), 150},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstTokenMsValue(tt.start, tt.firstTokenAt)
			if got != tt.want {
				t.Errorf("firstTokenMsValue() = %d, want %d", got, tt.want)
			}
		})
	}
}
