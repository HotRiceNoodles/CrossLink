package middleware

import (
	"testing"
)

func TestRouteTypeFromPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/messages/extra", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/embeddings", "embeddings"},
		{"/admin/api/playground", "playground"},
		{"/v1/images/generations", "images"},
		{"/v1/audio/speech", "audio"},
		{"/v1/batch", "batch"},
		{"/admin/api/users", ""},
		{"/unknown/path", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := routeTypeFromPath(tt.path)
			if got != tt.want {
				t.Errorf("routeTypeFromPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestStr(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  string
	}{
		{"nil", nil, ""},
		{"string", "hello", "hello"},
		{"int", 42, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := str(tt.input)
			if got != tt.want {
				t.Errorf("str(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"int", 42, 42},
		{"int64", int64(42), 42},
		{"float64", float64(42.5), 42},
		{"nil", nil, 0},
		{"string", "abc", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toInt(tt.input)
			if got != tt.want {
				t.Errorf("toInt(%v) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}
