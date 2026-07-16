package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crosslink/internal/domain"
	"github.com/crosslink/internal/model"
	"gorm.io/datatypes"
)

func newMockProvider(responseText string, delayMs int) *MockProvider {
	return &MockProvider{name: "test-mock", responseText: responseText, delayMs: delayMs}
}

func mockReq(msgs ...string) *domain.OpenAIRequest {
	var messages []domain.OpenAIMessage
	for _, m := range msgs {
		messages = append(messages, domain.OpenAIMessage{Role: "user", Content: m})
	}
	return &domain.OpenAIRequest{Model: "mock-gpt-4o", Messages: messages}
}

func TestMockChat_ReturnsValidResponse(t *testing.T) {
	p := newMockProvider("", 0)
	resp, err := p.Chat(context.Background(), mockReq("hello"), "")
	if err != nil {
		t.Fatalf("Chat error: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %s, want chat.completion", resp.Object)
	}
	if resp.Model != "mock-gpt-4o" {
		t.Errorf("model = %s, want mock-gpt-4o", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices len = %d, want 1", len(resp.Choices))
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %s, want assistant", resp.Choices[0].Message.Role)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %s, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Error("total_tokens should be > 0")
	}
}

func TestMockChat_FixedMode_NoUserContentLeak(t *testing.T) {
	p := newMockProvider("", 0)
	resp, _ := p.Chat(context.Background(), mockReq("my secret password is 12345"), "")
	content := domain.ContentText(resp.Choices[0].Message.Content)
	if strings.Contains(content, "secret password") {
		t.Errorf("fixed mode must NOT echo user content, got: %s", content)
	}
	if !strings.Contains(content, "mock-gpt-4o") {
		t.Errorf("should contain model name, got: %s", content)
	}
}

func TestMockChat_CustomText(t *testing.T) {
	p := newMockProvider("Hello from mock!", 0)
	resp, _ := p.Chat(context.Background(), mockReq("hi"), "")
	content := domain.ContentText(resp.Choices[0].Message.Content)
	if content != "Hello from mock!" {
		t.Errorf("content = %q, want 'Hello from mock!'", content)
	}
}

func TestMockChat_TokenEstimation(t *testing.T) {
	p := newMockProvider("", 0)
	resp, _ := p.Chat(context.Background(), mockReq("你好世界"), "")
	if resp.Usage.PromptTokens < 2 {
		t.Errorf("prompt_tokens = %d, CJK text should estimate >= 2 tokens", resp.Usage.PromptTokens)
	}
}

func TestMockChat_EmptyMessages(t *testing.T) {
	p := newMockProvider("", 0)
	resp, err := p.Chat(context.Background(), &domain.OpenAIRequest{Model: "m"}, "")
	if err != nil {
		t.Fatalf("empty messages should not error: %v", err)
	}
	content := domain.ContentText(resp.Choices[0].Message.Content)
	if !strings.Contains(content, "0 messages") && !strings.Contains(content, "Received") {
		t.Errorf("should mention message count, got: %s", content)
	}
}

func TestMockStream_EmitsChunksThenDone(t *testing.T) {
	p := newMockProvider("This is a longer mock response that will be split into parts.", 0)
	ch, err := p.StreamChat(context.Background(), mockReq("hi"), "")
	if err != nil {
		t.Fatalf("StreamChat error: %v", err)
	}
	var chunks []domain.SSEChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected >= 2 chunks (content + done), got %d", len(chunks))
	}
	if !chunks[len(chunks)-1].Done {
		t.Error("last chunk must be Done=true")
	}
}

func TestMockStream_ChunkStructure(t *testing.T) {
	p := newMockProvider("ABCDEF", 0)
	ch, _ := p.StreamChat(context.Background(), mockReq("hi"), "")
	var chunks []domain.SSEChunk
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	// First content chunk should have role=assistant
	for _, c := range chunks {
		if c.Chunk != nil && len(c.Chunk.Choices) > 0 {
			if c.Chunk.Choices[0].Delta.Role == "assistant" {
				break // found role chunk
			}
		}
	}
	// Last non-done chunk should have finish_reason
	var lastChunk *domain.OpenAIChunk
	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].Chunk != nil {
			lastChunk = chunks[i].Chunk
			break
		}
	}
	if lastChunk == nil {
		t.Fatal("no content chunk found")
	}
	if len(lastChunk.Choices) == 0 || lastChunk.Choices[0].FinishReason == nil {
		t.Error("last content chunk should have finish_reason")
	}
}

func TestMockStream_ContextCancel(t *testing.T) {
	p := newMockProvider("test response", 5000) // 5s delay per chunk
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := p.StreamChat(ctx, mockReq("hi"), "")

	// Cancel after 50ms — should stop the goroutine quickly.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	done := false
	for range ch {
		// drain
	}
	done = true
	if !done {
		t.Error("channel should close after context cancel")
	}
}

func TestMock_Delay(t *testing.T) {
	p := newMockProvider("", 100) // 100ms delay
	start := time.Now()
	p.Chat(context.Background(), mockReq("hi"), "")
	elapsed := time.Since(start)
	if elapsed < 90*time.Millisecond {
		t.Errorf("delay not applied: elapsed = %v, want >= 90ms", elapsed)
	}
}

func TestMock_Registered(t *testing.T) {
	mp := &model.Provider{Name: "reg-test", AdapterType: "mock"}
	prov, err := CreateProvider(mp, 10*time.Second)
	if err != nil {
		t.Fatalf("CreateProvider(mock) error: %v", err)
	}
	if _, ok := prov.(*MockProvider); !ok {
		t.Errorf("expected *MockProvider, got %T", prov)
	}
}

func TestMock_AdapterMeta(t *testing.T) {
	adapters := ListAdapters()
	found := false
	for _, a := range adapters {
		if a.Type == "mock" {
			found = true
			if a.Meta.DisplayName == "" {
				t.Error("mock adapter meta has empty display name")
			}
		}
	}
	if !found {
		t.Error("mock adapter not found in ListAdapters")
	}
}

func TestParseMockConfig_NilEmpty(t *testing.T) {
	rt, delay := parseMockConfig(nil)
	if rt != "" || delay != 0 {
		t.Errorf("nil config should return ('', 0), got (%q, %d)", rt, delay)
	}
	rt, delay = parseMockConfig(datatypes.JSON([]byte(`{}`)))
	if rt != "" || delay != 0 {
		t.Errorf("empty JSON should return ('', 0), got (%q, %d)", rt, delay)
	}
}

func TestParseMockConfig_Invalid(t *testing.T) {
	rt, delay := parseMockConfig(datatypes.JSON([]byte(`not json`)))
	if rt != "" || delay != 0 {
		t.Errorf("invalid JSON should return ('', 0), got (%q, %d)", rt, delay)
	}
}

func TestSplitText_RuneSafe(t *testing.T) {
	parts := splitText("你好世界测试", 3)
	for _, p := range parts {
		for _, r := range p {
			if r == 0xFFFD {
				t.Errorf("replacement char found in part: %q", p)
			}
		}
	}
	// Short text should not split
	short := splitText("AB", 4)
	if len(short) != 1 {
		t.Errorf("short text should return 1 part, got %d", len(short))
	}
}
