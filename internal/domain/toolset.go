package domain

// ToolObservation carries one gateway request's tool evidence to the
// ToolObserver seam. It is built synchronously inside the usage-entry closure
// (same lifecycle as context analysis) and processed asynchronously by the
// overlay-side collector. Schema hashing, truncation and persistence all
// happen on the observer side, never on the request path.
type ToolObservation struct {
	APIKeyID int64
	TeamID   int64
	OrgID    int64
	Source   string // "proxy" for now; reserved for playground/replay later
	Protocol string // "openai" | "anthropic"
	Model    string // actually-served model
	// Declared are the tools carried by the request's tools[] definition.
	// Builders cap the slice at 128 entries to bound queue memory.
	Declared []DeclaredTool
	// Invoked are tool names actually emitted by the model in this response
	// (tool_use / tool_calls). Empty when the response was a cache replay.
	Invoked []string
}

// DeclaredTool is one tool definition as seen on the wire. SchemaRaw is the
// untouched JSON schema (OpenAI parameters / Anthropic input_schema); the
// observer reduces it to sha256 + byte size and never persists the raw bytes.
type DeclaredTool struct {
	Name        string
	Description string
	SchemaRaw   []byte
}

// ToolObserver receives per-request tool observations for agent toolset
// profiling. The commercial overlay implements it (Pro tier and above);
// Community builds leave it nil and handlers skip observation entirely.
type ToolObserver interface {
	Observe(obs ToolObservation)
}
