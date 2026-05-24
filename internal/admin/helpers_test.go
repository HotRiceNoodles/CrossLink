package admin

import (
	"testing"

	"github.com/crosslink/internal/model"
)

func TestSanitizeURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"simple webhook", "https://example.com/webhook", "https://example.com/%2A%2A%2A"},
		{"with credentials", "https://user:pass@example.com/hook", "https://%2A%2A%2A:%2A%2A%2A@example.com/%2A%2A%2A"},
		{"with query string", "https://example.com/hook?secret=abc", "https://example.com/%2A%2A%2A"},
		{"root path with slash", "https://example.com/", "https://example.com/"},
		{"no path", "https://example.com", "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeURL(tt.raw); got != tt.want {
				t.Errorf("sanitizeURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestIsAdminUser(t *testing.T) {
	tests := []struct {
		name string
		user *model.User
		want bool
	}{
		{
			name: "admin role",
			user: &model.User{RoleID: 1, Role: model.Role{ID: 1, Name: model.RoleAdmin}},
			want: true,
		},
		{
			name: "viewer role",
			user: &model.User{RoleID: 2, Role: model.Role{ID: 2, Name: "viewer"}},
			want: false,
		},
		{
			name: "zero-value role",
			user: &model.User{RoleID: 0, Role: model.Role{}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAdminUser(tt.user); got != tt.want {
				t.Errorf("isAdminUser() = %v, want %v", got, tt.want)
			}
		})
	}
}
