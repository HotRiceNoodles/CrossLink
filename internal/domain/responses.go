package domain

import "encoding/json"

// ResponsesRequest models the OpenAI Responses API request body (/v1/responses).
// Input is kept as RawMessage because it is polymorphic (string or []ResponsesInputItem);
// 3A passthrough forwards the raw bytes untouched, 3B translation parses it.
type ResponsesRequest struct {
	Model               string          `json:"model"`
	Input               json.RawMessage `json:"input"`
	Instructions        string          `json:"instructions,omitempty"`
	MaxOutputTokens     *int            `json:"max_output_tokens,omitempty"`
	PreviousResponseID  string          `json:"previous_response_id,omitempty"`
	Store               *bool           `json:"store,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	Tools               []ResponsesTool `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Reasoning           json.RawMessage `json:"reasoning,omitempty"`
	User                string          `json:"user,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

// ResponsesTool is a tool definition in the Responses API (type is always "function").
type ResponsesTool struct {
	Type     string             `json:"type"`
	Function ResponsesToolFunc  `json:"function"`
}

type ResponsesToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ResponsesInputItem covers the item types that may appear when Input is an array.
// Only the fields relevant to 3B translation are modeled.
type ResponsesInputItem struct {
	Type      string          `json:"type"`                 // message | function_call | function_call_output | reasoning
	Role      string          `json:"role,omitempty"`       // message
	Content   json.RawMessage `json:"content,omitempty"`    // message: string or []part
	CallID    string          `json:"call_id,omitempty"`    // function_call / function_call_output
	Name      string          `json:"name,omitempty"`       // function_call
	Arguments string          `json:"arguments,omitempty"`  // function_call
	Output    string          `json:"output,omitempty"`     // function_call_output
	ID        string          `json:"id,omitempty"`         // function_call (carried id)
}

// ResponsesOutputItem models an item in the Responses response Output array.
type ResponsesOutputItem struct {
	Type      string                 `json:"type"`              // message | function_call
	ID        string                 `json:"id,omitempty"`
	Role      string                 `json:"role,omitempty"`    // message
	Status    string                 `json:"status,omitempty"`
	Content   []ResponsesContentPart `json:"content,omitempty"` // message
	CallID    string                 `json:"call_id,omitempty"` // function_call
	Name      string                 `json:"name,omitempty"`    // function_call
	Arguments string                 `json:"arguments,omitempty"` // function_call
}

type ResponsesContentPart struct {
	Type string `json:"type"` // output_text | refusal_text
	Text string `json:"text,omitempty"`
}

// ResponsesResponse models the OpenAI Responses API non-streaming response.
// 3B reverse-translation produces this from an internal OpenAIResponse.
type ResponsesResponse struct {
	ID        string               `json:"id"`
	Object    string               `json:"object"` // "response"
	Status    string               `json:"status"` // completed | incomplete | failed
	Model     string               `json:"model,omitempty"`
	Output    []ResponsesOutputItem `json:"output"`
	Usage     ResponsesUsage       `json:"usage"`
	PreviousResponseID string      `json:"previous_response_id,omitempty"`
}

type ResponsesUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	InputTokensDetails  *ResponsesInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *ResponsesOutputTokensDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// ResponsesEvent models a single Responses streaming event written as SSE.
// Wire format: "event: <Type>\ndata: <json payload>\n\n".
type ResponsesEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"-"` // marshaled inline into the SSE data line
}
