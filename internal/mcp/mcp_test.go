package mcp

import (
	"encoding/json"
	"testing"
)

func TestJSONRPCRequest_ParseMethod(t *testing.T) {
	raw := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	var req JSONRPCRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", req.JSONRPC)
	}
	if req.Method != MethodToolsList {
		t.Errorf("method = %q, want %q", req.Method, MethodToolsList)
	}
}

func TestJSONRPCResponse_Marshal(t *testing.T) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Result:  map[string]interface{}{"tools": []interface{}{}},
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(data) != `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` {
		t.Errorf("unexpected output: %s", data)
	}
}

func TestValidateServerName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "github-mcp", false},
		{"valid numbers", "slack123", false},
		{"too short", "ab", true},
		{"too long", "a-very-long-server-name-that-exceeds-the-sixty-four-character-limit-xxx", true},
		{"spaces", "my server", true},
		{"slashes", "my/server", true},
		{"dots", "my.server", true},
		{"starts with dash", "-server", true},
		{"ends with dash", "server-", true},
		{"uppercase", "MyServer", true},
		{"empty", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateServerName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateServerName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
