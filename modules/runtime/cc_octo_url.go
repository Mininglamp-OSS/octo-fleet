package runtime

import (
	"net/url"
	"strings"
)

// isAllowedGatewayURL validates gateway URL policy for cc-octo install:
//   - https://any-host → allowed
//   - http://localhost or http://127.0.0.1 (with optional port) → allowed
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
