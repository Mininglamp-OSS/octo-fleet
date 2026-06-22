package runtime

import (
	"net"
	"net/url"
	"strings"
)

// isPrivateOrLoopbackIPv4 returns true when host is a private or loopback IPv4
// literal: 127.0.0.0/8, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16,
// 169.254.0.0/16, or 100.64.0.0/10. Non-IPv4 hosts return false.
func isPrivateOrLoopbackIPv4(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 127.0.0.0/8
	if ip4[0] == 127 {
		return true
	}
	// 10.0.0.0/8
	if ip4[0] == 10 {
		return true
	}
	// 172.16.0.0/12
	if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
		return true
	}
	// 192.168.0.0/16
	if ip4[0] == 192 && ip4[1] == 168 {
		return true
	}
	// 169.254.0.0/16
	if ip4[0] == 169 && ip4[1] == 254 {
		return true
	}
	// 100.64.0.0/10
	if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return true
	}
	return false
}

// isAllowedGatewayURL validates gateway URL shape for cc-octo install tasks.
// This is a fast-fail UX check that rejects common private/loopback IPv4
// literals over https (which cc-channel-octo's authoritative SSRF policy would
// block anyway). The full IPv6 / v4-mapped / NAT64 matrix is enforced by
// cc-channel-octo's isAllowedApiUrl at the point the gateway URL is consumed.
//
// Policy:
//   - https://host → allowed UNLESS host is a private/loopback IPv4 literal
//   - http://localhost or http://127.0.0.1 (with optional port) → allowed (local dev)
//   - everything else → rejected
func isAllowedGatewayURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}

	switch u.Scheme {
	case "https":
		host := u.Hostname()
		if isPrivateOrLoopbackIPv4(host) {
			return false
		}
		return true
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" {
			return true
		}
		return false
	default:
		return false
	}
}
