package middleware

import "encoding/json"

// 在请求脱敏前固定原始用户轮次，保留语义审核所需上下文。
func originalTurnPrompt(body []byte) string {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input  json.RawMessage `json:"input"`
		Prompt string          `json:"prompt"`
	}
	if json.Unmarshal(body, &request) != nil {
		return ""
	}
	extract := func(raw json.RawMessage) string {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			for _, part := range parts {
				if part.Type == "text" || part.Type == "input_text" {
					text += part.Text
				}
			}
		}
		return text
	}
	for i := len(request.Messages) - 1; i >= 0; i-- {
		if request.Messages[i].Role == "user" {
			return extract(request.Messages[i].Content)
		}
	}
	if text := extract(request.Input); text != "" {
		return text
	}
	var input []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(request.Input, &input) == nil {
		for i := len(input) - 1; i >= 0; i-- {
			if input[i].Role == "user" {
				return extract(input[i].Content)
			}
		}
	}
	return request.Prompt
}
