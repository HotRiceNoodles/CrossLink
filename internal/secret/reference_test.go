package secret

import "testing"

func TestIsReference(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"sk-abc123", false},
		{"env://MY_KEY", true},
		{"enc://YWJjZGVm", true},
		{"vault://secret/data/key", true},
		{"aws-sm://my-secret", true},
		{"http://example.com", false},
		{"https://example.com", false},
		{"HTTP://example.com", true},
		{"://empty-scheme", false},
	}
	for _, tt := range tests {
		if got := IsReference(tt.input); got != tt.want {
			t.Errorf("IsReference(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParseScheme(t *testing.T) {
	scheme, key, ok := ParseScheme("env://MY_KEY")
	if !ok || scheme != "env" || key != "MY_KEY" {
		t.Errorf("ParseScheme(env://MY_KEY) = %q, %q, %v; want env, MY_KEY, true", scheme, key, ok)
	}

	scheme, key, ok = ParseScheme("vault://secret/data/llm#field")
	if !ok || scheme != "vault" || key != "secret/data/llm#field" {
		t.Errorf("ParseScheme(vault://...) = %q, %q, %v", scheme, key, ok)
	}

	_, _, ok = ParseScheme("plaintext")
	if ok {
		t.Error("ParseScheme(plaintext) should return ok=false")
	}

	_, _, ok = ParseScheme("")
	if ok {
		t.Error("ParseScheme('') should return ok=false")
	}

	_, _, ok = ParseScheme("://no-scheme")
	if ok {
		t.Error("ParseScheme('://no-scheme') should return ok=false")
	}
}
