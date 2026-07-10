package admin

import (
	"context"
	"net"
	"net/url"
	"time"
)

// providerHostLookupTimeout bounds the DNS lookup in isInternalHost so a slow or
// unresponsive resolver cannot hang an admin provider-create/update request.
const providerHostLookupTimeout = 5 * time.Second

// isInternalHost blocks SSRF targets: loopback, link-local, private ranges, unspecified.
// It also resolves hostnames via DNS (bounded by providerHostLookupTimeout) to catch
// names that point to internal IPs.
func isInternalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ctx, cancel := context.WithTimeout(context.Background(), providerHostLookupTimeout)
		defer cancel()
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return true
		}
		for _, resolved := range ips {
			if isInternalIP(resolved.IP) {
				return true
			}
		}
		return false
	}
	return isInternalIP(ip)
}

func isInternalIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

func sanitizeURL(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = url.UserPassword("***", "***")
	}
	if u.Path != "" && u.Path != "/" {
		u.Path = "/***"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
