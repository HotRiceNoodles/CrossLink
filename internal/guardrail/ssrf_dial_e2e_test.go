package guardrail

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Real-socket end-to-end: an HTTP server bound to one of THIS machine's actual
// private interface addresses, reached through NewSSRFSafeDialer (the same
// dialer the provider transports use). No test seam, no global switch — the
// allowlist is what permits the connection.
func firstPrivateIPv4(t *testing.T) net.IP {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil {
				continue
			}
			if ip4.IsPrivate() {
				return ip4
			}
		}
	}
	t.Skip("no private IPv4 interface address on this machine")
	return nil
}

func TestSSRFDialer_RealPrivateAddress_AllowlistEndToEnd(t *testing.T) {
	restoreStrict(t)
	ip := firstPrivateIPv4(t)

	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot bind private address %s: %v", ip, err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok-private")
	}))
	server.Listener = ln
	server.Start()
	defer server.Close()

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: NewSSRFSafeDialer(3 * time.Second)},
	}

	// Strict default: the connection to the private address is refused.
	resp, err := client.Get(server.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatalf("strict default allowed connection to private %s", ip)
	}

	// Allowlisting exactly this /32 permits the real connection.
	if err := SetInternalAllowlist([]string{ip.String() + "/32"}); err != nil {
		t.Fatalf("allowlist %s/32: %v", ip, err)
	}
	resp, err = client.Get(server.URL)
	if err != nil {
		t.Fatalf("allowlisted dial to %s failed: %v", ip, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted dial status = %d", resp.StatusCode)
	}

	// Dropping the allowlist again re-blocks it (strict restore verified).
	if err := SetInternalAllowlist(nil); err != nil {
		t.Fatalf("clear allowlist: %v", err)
	}
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("connection to private address allowed after allowlist cleared")
	}
}
