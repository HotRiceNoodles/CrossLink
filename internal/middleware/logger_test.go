package middleware

import (
	"testing"
)

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/admin/api/keys/123", "/admin/api/keys/:id"},
		{"/admin/api/keys/abc", "/admin/api/keys/abc"},
		{"/v1/messages", "/v1/messages"},
		{"/admin/users/42/keys/99", "/admin/users/:id/keys/:id"},
		{"", ""},
		{"/", "/"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := sanitizePath(tt.path)
			if got != tt.want {
				t.Errorf("sanitizePath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
