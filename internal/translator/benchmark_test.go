package translator

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/crosslink/internal/domain"
)


// --- Anthropic -> OpenAI request benchmark ---

func BenchmarkAnthropicToOpenAI(b *testing.B) {
	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 4096,
		System:    json.RawMessage(`"You are a helpful coding assistant. Always respond with well-structured code and explanations."`),
		Messages: []domain.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"Write a Go function that calculates the Fibonacci sequence up to N terms. Include error handling for negative inputs."`)},
			{Role: "assistant", Content: json.RawMessage(`"Here is a Go function that calculates the Fibonacci sequence up to N terms with error handling for negative inputs. It returns a slice of integers and an error. The function handles edge cases for n=0 and n=1, and uses a simple iterative approach with O(n) time complexity."`)},
			{Role: "user", Content: json.RawMessage(`"Can you add memoization to make it more efficient for repeated calls? Also add a benchmark test."`)},
		},
		Tools: json.RawMessage(`[
			{"name":"run_code","description":"Execute Go code and return the output","input_schema":{"type":"object","properties":{"code":{"type":"string","description":"Go source code to execute"},"timeout":{"type":"integer","description":"Timeout in seconds"}},"required":["code"]}},
			{"name":"search_docs","description":"Search documentation for a given query","input_schema":{"type":"object","properties":{"query":{"type":"string","description":"Search query"},"language":{"type":"string","description":"Programming language filter"}},"required":["query"]}}
		]`),
		ToolChoice: json.RawMessage(`{"type":"auto"}`),
		Metadata:   &domain.AnthropicMetadata{UserID: "user-benchmark-001"},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := AnthropicToOpenAI(req, "deepseek-chat")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- OpenAI -> Anthropic response benchmark ---

func BenchmarkOpenAIToAnthropic(b *testing.B) {
	resp := &domain.OpenAIResponse{
		ID:     "chatcmpl-benchmark-001",
		Object: "chat.completion",
		Model:  "deepseek-chat",
		Choices: []domain.OpenAIChoice{
			{
				Index:        0,
				FinishReason: "tool_calls",
				Message: domain.OpenAIMessage{
					Role:    "assistant",
					Content: "Let me search the documentation for that.",
					ToolCalls: []domain.OpenAIToolCall{
						{
							ID:   "call_abc123",
							Type: "function",
							Function: domain.OpenAIFunctionCall{
								Name:      "search_docs",
								Arguments: `{"query":"Go memoization patterns","language":"go"}`,
							},
						},
						{
							ID:   "call_def456",
							Type: "function",
							Function: domain.OpenAIFunctionCall{
								Name:      "run_code",
								Arguments: `{"code":"package main\n\nimport \"fmt\"\n\nfunc main() {\n    fmt.Println(\"test\")\n}","timeout":10}`,
							},
						},
					},
				},
			},
		},
		Usage: domain.OpenAIUsage{
			PromptTokens:     512,
			CompletionTokens: 128,
			TotalTokens:      640,
		},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := OpenAIToAnthropic(resp, "claude-sonnet-4-20250514")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- Large messages stress test (50 messages) ---

func BenchmarkAnthropicToOpenAI_LargeMessages(b *testing.B) {
	messages := make([]domain.AnthropicMessage, 0, 50)

	// Simulate a long conversation with alternating user/assistant messages.
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			messages = append(messages, domain.AnthropicMessage{
				Role:    "user",
				Content: json.RawMessage(fmt.Sprintf(`"This is user message number %d. It contains a reasonably long question about Go concurrency patterns, including goroutines, channels, and sync primitives. We want to understand the best practices for building high-throughput data pipelines."`, i+1)),
			})
		} else {
			// Every 5th assistant message includes a tool_use block.
			if i%10 == 5 {
				messages = append(messages, domain.AnthropicMessage{
					Role:    "assistant",
					Content: json.RawMessage(fmt.Sprintf(`[{"type":"text","text":"Let me run that code for you."},{"type":"tool_use","id":"toolu_%d","name":"run_code","input":{"code":"package main\n\nimport \"fmt\"\n\nfunc main() {\n    ch := make(chan int, 10)\n    go func() { ch <- 42 }()\n    fmt.Println(<-ch)\n}","timeout":5}}]`, i)),
				})
				// Follow with a tool_result from user.
				i++ // advance to keep alternating correctly
				messages = append(messages, domain.AnthropicMessage{
					Role:    "user",
					Content: json.RawMessage(fmt.Sprintf(`[{"type":"tool_result","tool_use_id":"toolu_%d","content":"42\n\nProgram exited."}]`, i-1)),
				})
			} else {
				messages = append(messages, domain.AnthropicMessage{
					Role:    "assistant",
					Content: json.RawMessage(fmt.Sprintf(`"This is assistant response number %d. Here is a detailed explanation of Go concurrency patterns including worker pools, fan-out/fan-in, pipeline patterns, and context-based cancellation. The key insight is to use channels for communication and select for multiplexing."`, i+1)),
				})
			}
		}
	}

	req := &domain.AnthropicRequest{
		Model:     "claude-sonnet-4-20250514",
		MaxTokens: 8192,
		System:    json.RawMessage(`"You are a helpful coding assistant with expertise in Go, systems programming, and distributed systems."`),
		Messages:  messages,
		Tools: json.RawMessage(`[
			{"name":"run_code","description":"Execute Go code and return the output","input_schema":{"type":"object","properties":{"code":{"type":"string"},"timeout":{"type":"integer"}},"required":["code"]}},
			{"name":"search_docs","description":"Search documentation","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
		]`),
		ToolChoice: json.RawMessage(`{"type":"auto"}`),
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := AnthropicToOpenAI(req, "deepseek-chat")
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- OpenAI -> Anthropic request (reverse request) benchmark ---

func BenchmarkReverseRequest(b *testing.B) {
	maxTokens := 4096
	temp := 0.7
	topP := 0.9

	req := &domain.OpenAIRequest{
		Model:       "gpt-4o",
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		TopP:        &topP,
		Stream:      true,
		Tools: []domain.OpenAITool{
			{
				Type: "function",
				Function: domain.OpenAIFunctionDef{
					Name:        "get_weather",
					Description: "Get the current weather for a city",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["city"]}`),
				},
			},
			{
				Type: "function",
				Function: domain.OpenAIFunctionDef{
					Name:        "search_web",
					Description: "Search the web for information",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"Search query"},"num_results":{"type":"integer","description":"Number of results to return"}},"required":["query"]}`),
				},
			},
		},
		ToolChoice: "auto",
		Stop:       []string{"END", "STOP"},
		User:       "user-benchmark-002",
		Messages: []domain.OpenAIMessage{
			{Role: "system", Content: "You are a helpful assistant with access to weather and web search tools."},
			{Role: "user", Content: "What's the weather in Tokyo and what are the top news stories today?"},
			{Role: "assistant", Content: "Let me check both for you.", ToolCalls: []domain.OpenAIToolCall{
				{ID: "call_wx_001", Type: "function", Function: domain.OpenAIFunctionCall{Name: "get_weather", Arguments: `{"city":"Tokyo","unit":"celsius"}`}},
				{ID: "call_src_001", Type: "function", Function: domain.OpenAIFunctionCall{Name: "search_web", Arguments: `{"query":"top news stories today","num_results":5}`}},
			}},
			{Role: "tool", ToolCallID: "call_wx_001", Content: "Tokyo: 22°C, partly cloudy, humidity 65%"},
			{Role: "tool", ToolCallID: "call_src_001", Content: "1. Tech breakthrough in quantum computing\n2. Global climate summit results\n3. New trade agreement signed\n4. Space mission update\n5. AI regulation proposals"},
			{Role: "assistant", Content: "Here's what I found:\n\n**Weather in Tokyo:** 22°C, partly cloudy with 65% humidity.\n\n**Top News:**\n1. Quantum computing breakthrough\n2. Climate summit results\n3. New trade agreement\n4. Space mission update\n5. AI regulation proposals"},
		},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := OpenAIToAnthropicRequest(req)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- Anthropic -> OpenAI response (reverse response) benchmark ---

func BenchmarkReverseResponse(b *testing.B) {
	resp := &domain.AnthropicResponse{
		ID:   "msg_benchmark_001",
		Type: "message",
		Role: "assistant",
		Content: []domain.ContentBlock{
			{Type: "thinking", Text: "The user wants to know about Go concurrency. I should explain goroutines, channels, and sync primitives with code examples."},
			{Type: "text", Text: "Here's a comprehensive guide to Go concurrency patterns:\n\n## Goroutines\n\nGoroutines are lightweight threads managed by the Go runtime. They're cheap to create (a few KB of stack) and multiplexed onto OS threads.\n\n```go\ngo func() {\n    fmt.Println(\"running concurrently\")\n}()\n```\n\n## Channels\n\nChannels provide typed, synchronized communication between goroutines.\n\n```go\nch := make(chan int, 10) // buffered\nch <- 42\nvalue := <-ch\n```\n\n## Worker Pool Pattern\n\n```go\nfunc worker(id int, jobs <-chan int, results chan<- int) {\n    for j := range jobs {\n        results <- j * 2\n    }\n}\n```\n\nThis pattern is ideal for rate-limiting concurrent work."},
			{Type: "tool_use", ID: "toolu_bench_001", Name: "run_code", Input: json.RawMessage(`{"code":"package main\n\nimport (\n    \"fmt\"\n    \"sync\"\n    \"time\"\n)\n\nfunc worker(id int, jobs <-chan int, results chan<- int) {\n    for j := range jobs {\n        time.Sleep(time.Millisecond * 100)\n        results <- j * 2\n    }\n}\n\nfunc main() {\n    jobs := make(chan int, 100)\n    results := make(chan int, 100)\n\n    var wg sync.WaitGroup\n    for w := 0; w < 3; w++ {\n        wg.Add(1)\n        go func(id int) {\n            defer wg.Done()\n            worker(id, jobs, results)\n        }(w)\n    }\n\n    for j := 0; j < 5; j++ {\n        jobs <- j\n    }\n    close(jobs)\n    wg.Wait()\n    close(results)\n\n    for r := range results {\n        fmt.Println(r)\n    }\n}","timeout":10}`)},
		},
		Model:      "claude-sonnet-4-20250514",
		StopReason: "tool_use",
		Usage:      domain.AnthropicUsage{InputTokens: 1024, OutputTokens: 512},
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := AnthropicToOpenAIResponse(resp)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// --- StreamTranslator: OpenAI chunks -> Anthropic SSE events ---

func BenchmarkStreamTranslator_Chunks(b *testing.B) {
	stopReason := "stop"

	chunks := []domain.SSEChunk{
		// First chunk triggers message_start
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Role: "assistant"}},
			},
		}},
		// Content deltas
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: "Here is a Go function"}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " that calculates the Fibonacci"}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " sequence using dynamic programming."}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " It runs in O(n) time and O(n) space."}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " Here is the code:\n\n```go\nfunc fib(n int) []int {\n    if n <= 0 { return nil }\n    dp := make([]int, n)\n    dp[0], dp[1] = 0, 1\n    for i := 2; i < n; i++ {\n        dp[i] = dp[i-1] + dp[i-2]\n    }\n    return dp\n}\n```"}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " This implementation handles edge cases and returns the full sequence up to n terms."}},
			},
		}},
		// Finish chunk
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-stream", Object: "chat.completion.chunk", Model: "deepseek-chat",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{}, FinishReason: &stopReason},
			},
			Usage: &domain.OpenAIChunkUsage{PromptTokens: 256, CompletionTokens: 128, TotalTokens: 384},
		}},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		st := NewStreamTranslator("msg_bench_stream", "claude-sonnet-4-20250514", 100)
		for _, c := range chunks {
			st.TranslateChunk(c)
		}
	}
}

// --- ReverseStreamTranslator: Anthropic SSE events -> OpenAI chunks ---

func BenchmarkReverseStreamTranslator_Events(b *testing.B) {
	events := []struct {
		eventType string
		data      []byte
	}{
		{"message_start", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":    "msg_bench_rev",
					"model": "claude-sonnet-4-20250514",
					"usage": map[string]any{"input_tokens": 200},
				},
			})
			return d
		}()},
		{"content_block_start", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": 0,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})
			return d
		}()},
		{"content_block_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": "Here is a Go function"},
			})
			return d
		}()},
		{"content_block_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": " that calculates the Fibonacci"},
			})
			return d
		}()},
		{"content_block_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": " sequence with O(n) complexity."},
			})
			return d
		}()},
		{"content_block_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": " It handles edge cases for n<=0."},
			})
			return d
		}()},
		{"content_block_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": " Full implementation below."},
			})
			return d
		}()},
		{"content_block_stop", func() []byte {
			d, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
			return d
		}()},
		{"message_delta", func() []byte {
			d, _ := json.Marshal(map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": map[string]any{"output_tokens": 150},
			})
			return d
		}()},
		{"message_stop", func() []byte {
			d, _ := json.Marshal(map[string]any{"type": "message_stop"})
			return d
		}()},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		tl := NewReverseStreamTranslator()
		for _, evt := range events {
			tl.TranslateEvent(evt.eventType, evt.data)
		}
	}
}

// --- StreamTranslator with thinking blocks ---

func BenchmarkStreamTranslator_WithThinking(b *testing.B) {
	stopReason := "stop"

	chunks := []domain.SSEChunk{
		// First chunk: message_start
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Role: "assistant"}},
			},
		}},
		// Thinking/reasoning content
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: "Let me analyze the Fibonacci problem."}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: " The iterative approach is optimal with O(n) time."}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{ReasoningContent: " Edge cases: n=0 returns empty, n=1 returns [0]."}},
			},
		}},
		// Transition to text content (thinking block closes, text block starts)
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: "Here is the optimized Fibonacci implementation:"}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: "\n\n```go\nfunc fib(n int) []int {\n    if n <= 0 { return nil }\n    dp := make([]int, n)\n    dp[0] = 0\n    if n > 1 { dp[1] = 1 }\n    for i := 2; i < n; i++ {\n        dp[i] = dp[i-1] + dp[i-2]\n    }\n    return dp\n}\n```"}},
			},
		}},
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{Content: " This runs in O(n) time and handles all edge cases."}},
			},
		}},
		// Finish
		{Chunk: &domain.OpenAIChunk{
			ID: "chatcmpl-bench-think", Object: "chat.completion.chunk", Model: "deepseek-reasoner",
			Choices: []domain.OpenAIChunkChoice{
				{Index: 0, Delta: domain.OpenAIChunkDelta{}, FinishReason: &stopReason},
			},
			Usage: &domain.OpenAIChunkUsage{PromptTokens: 256, CompletionTokens: 200, TotalTokens: 456},
		}},
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		st := NewStreamTranslator("msg_bench_think", "claude-sonnet-4-20250514", 100)
		for _, c := range chunks {
			st.TranslateChunk(c)
		}
	}
}
