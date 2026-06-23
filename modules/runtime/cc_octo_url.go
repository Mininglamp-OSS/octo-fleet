package runtime

import (
	"net"
	"net/url"
	"strings"
)

// isAllowedGatewayURL validates gateway URL shape for cc-octo install tasks.
// It is a fast-fail check that mirrors the models-proxy SSRF policy for any IP
// LITERAL host (see isUnsafeIP: IPv4/IPv6 loopback, private, link-local,
// unspecified, v4-mapped, NAT64, CGNAT). Domain names are not resolved here —
// they are gated authoritatively by cc-channel-octo's isAllowedApiUrl when the
// gateway URL is actually consumed.
//
// Policy:
//   - https://host → allowed unless host is localhost or an unsafe IP literal
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

	host := u.Hostname()
	switch u.Scheme {
	case "https":
		// localhost over https is never a real gateway (and could front a local
		// mitm proxy) — reject it alongside unsafe IP literals.
		if strings.EqualFold(host, "localhost") {
			return false
		}
		if ip := net.ParseIP(host); ip != nil && isUnsafeIP(ip) {
			return false
		}
		return true
	case "http":
		// local dev only
		return host == "localhost" || host == "127.0.0.1"
	default:
		return false
	}
}
