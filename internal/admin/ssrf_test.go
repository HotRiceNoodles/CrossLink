package admin

import (
	"net"
	"testing"
)

func TestIsInternalIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Loopback
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},

		// Private ranges
		{"private 10.x", "10.0.0.1", true},
		{"private 172.16.x", "172.16.0.1", true},
		{"private 192.168.x", "192.168.1.1", true},

		// Link-local
		{"link-local", "169.254.1.1", true},

		// Unspecified
		{"unspecified v4", "0.0.0.0", true},
		{"unspecified v6", "::", true},

		// Multicast
		{"multicast v4", "224.0.0.1", true},
		{"multicast v6", "ff02::1", true},

		// Public
		{"public v4 8.8.8.8", "8.8.8.8", false},
		{"public v4 1.1.1.1", "1.1.1.1", false},
		{"public v6", "2001:4860:4860::8888", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("failed to parse IP %q", tt.ip)
			}
			if got := isInternalIP(ip); got != tt.want {
				t.Errorf("isInternalIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIsInternalHost(t *testing.T) {
	tests := []struct {
		name string
		host string
		want bool
	}{
		{"localhost", "localhost", true},
		{"loopback v4", "127.0.0.1", true},
		{"private 192.168", "192.168.1.1", true},
		{"private 10.x", "10.0.0.1", true},
		{"public 8.8.8.8", "8.8.8.8", false},
		{"unresolvable hostname", "this-host-does-not-exist-xyz123.invalid", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isInternalHost(tt.host); got != tt.want {
				t.Errorf("isInternalHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
