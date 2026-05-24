package guardrail

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"syscall"
	"time"
)

func isRestrictedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func ValidateURLSafety(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL host is empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("failed to resolve URL host %q: %w", host, err)
	}
	for _, addr := range ips {
		if isRestrictedIP(addr.IP) {
			return fmt.Errorf("URL host %q resolves to restricted address %s", host, addr.IP)
		}
	}
	if len(ips) == 0 {
		return fmt.Errorf("URL host %q resolved to no addresses", host)
	}
	return nil
}

type ssrfSafeDialer struct {
	dialer *net.Dialer
}

// NewSSRFSafeDialer returns a DialContext function that validates resolved IPs
// against restricted ranges (loopback, private, link-local, multicast, unspecified)
// and uses a Control callback to prevent DNS rebinding attacks.
func NewSSRFSafeDialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &ssrfSafeDialer{dialer: &net.Dialer{Timeout: timeout}}
	return d.DialContext
}

func (d *ssrfSafeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("ssrf: invalid address %q: %w", addr, err)
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf: failed to resolve %q: %w", host, err)
	}
	for _, ipAddr := range ips {
		if isRestrictedIP(ipAddr.IP) {
			return nil, fmt.Errorf("ssrf: host %q resolves to restricted address %s", host, ipAddr.IP)
		}
	}
	// Use Control callback to validate the actual destination IP at socket level,
	// preventing DNS rebinding between resolution and connection.
	dd := *d.dialer
	dd.Control = func(network, address string, c syscall.RawConn) error {
		h, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(h)
		if ip == nil {
			return fmt.Errorf("ssrf: non-IP destination %q", h)
		}
		if isRestrictedIP(ip) {
			return fmt.Errorf("ssrf: blocked connection to restricted address %s", ip)
		}
		return nil
	}
	return dd.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
}
