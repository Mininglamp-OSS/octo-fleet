package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// llmModelsReq is the body of POST /v1/runtimes/llm-models: the operator's LLM
// gateway url + key, used once to list the gateway's models for the install
// modal. Neither value is persisted.
type llmModelsReq struct {
	GatewayURL string `json:"gateway_url"`
	APIKey     string `json:"api_key"`
}

// parseModelIDs extracts model ids from an OpenAI/Anthropic-style
// `GET /v1/models` body: {"data":[{"id":"..."}]}. Pure for unit testing.
func parseModelIDs(body []byte) ([]string, error) {
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	ids := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// modelsBaseURL strips trailing slashes then a single trailing "/v1"
// (case-insensitive) from a gateway URL, so the caller can append "/v1/models"
// exactly once whether the gateway was given with or without the /v1 suffix.
// Pure for unit testing.
func modelsBaseURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	if len(base) >= 3 && strings.EqualFold(base[len(base)-3:], "/v1") {
		base = strings.TrimRight(base[:len(base)-3], "/")
	}
	return base
}

// isUnsafeIP rejects any address that must never be the target of a server-side
// proxy request (SSRF). Normalizes v4-mapped (::ffff:a.b.c.d) and unwraps NAT64
// (64:ff9b::/96) before testing, so an embedded private IPv4 cannot slip past.
func isUnsafeIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 100.64.0.0/10 — CGNAT / RFC6598 shared address space. NOT covered by
	// net.IP.IsPrivate (which is RFC1918 + ULA only), but it can route to
	// carrier/cloud internal infra, so reject it explicitly.
	if len(ip) == net.IPv4len && ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127 {
		return true
	}
	// NAT64 64:ff9b::/96 → unwrap the embedded IPv4 and re-check.
	if len(ip) == net.IPv6len && ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b &&
		ip[4] == 0 && ip[5] == 0 && ip[6] == 0 && ip[7] == 0 &&
		ip[8] == 0 && ip[9] == 0 && ip[10] == 0 && ip[11] == 0 {
		return isUnsafeIP(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
	}
	return false
}

// validateProxyURL is a fast-fail UX check. The AUTHORITATIVE SSRF gate is the
// dial-time IP check in newSafeProxyClient (which also defeats DNS rebinding).
func validateProxyURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid url")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("gateway must be https")
	}
	if u.Hostname() == "localhost" {
		return fmt.Errorf("localhost not allowed")
	}
	return nil
}

// newSafeProxyClient validates the resolved IP AT DIAL TIME and dials that exact
// IP, so a hostname that resolves safe-then-private (DNS rebinding) cannot slip
// through between a pre-check and the real connection. Redirects are refused —
// a 30x could otherwise bounce to an internal host.
//
// TLS is unaffected by the IP substitution: http.Transport derives the TLS
// ServerName from the request's hostname (the addr it hands to DialContext),
// not from the IP we connect to, so the certificate is still verified against
// the original domain and InsecureSkipVerify stays off.
func newSafeProxyClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects not allowed") },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil || len(ips) == 0 {
					return nil, fmt.Errorf("resolve %s failed", host)
				}
				for _, ip := range ips {
					if isUnsafeIP(ip) {
						return nil, fmt.Errorf("refusing to connect to non-public address %s", ip)
					}
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
			},
		},
	}
}

// fetchLLMModels (web scope) proxies the operator gateway's GET /v1/models so
// the install modal can offer a model dropdown without hitting browser CORS.
// The gateway url is SSRF-gated at dial time (see newSafeProxyClient).
func (rt *Runtime) fetchLLMModels(c *wkhttp.Context) {
	var req llmModelsReq
	if err := c.BindJSON(&req); err != nil || req.GatewayURL == "" || req.APIKey == "" {
		responseError(c, errcode.Validation)
		return
	}
	if err := validateProxyURL(req.GatewayURL); err != nil {
		responseErrorD(c, errcode.Validation, nil, err.Error())
		return
	}

	// scheme+host only, for diagnostics — never log path / query / api key.
	gwHost := req.GatewayURL
	if pu, perr := url.Parse(strings.TrimSpace(req.GatewayURL)); perr == nil {
		gwHost = pu.Scheme + "://" + pu.Host
	}

	client := rt.proxyClient
	if client == nil {
		client = newSafeProxyClient(10 * time.Second)
	}
	base := modelsBaseURL(req.GatewayURL)
	httpReq, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		responseError(c, errcode.InternalError)
		return
	}
	httpReq.Header.Set("x-api-key", req.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(httpReq)
	if err != nil {
		rt.Error("fetchLLMModels: upstream request failed", zap.String("gateway", gwHost), zap.Error(err))
		responseErrorD(c, errcode.Validation, nil, "could not reach the gateway")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		rt.Error("fetchLLMModels: upstream non-200", zap.String("gateway", gwHost), zap.Int("status", resp.StatusCode))
		responseErrorD(c, errcode.Validation, nil, fmt.Sprintf("gateway returned %d", resp.StatusCode))
		return
	}
	ids, err := parseModelIDs(body)
	if err != nil {
		rt.Error("fetchLLMModels: parse models", zap.String("gateway", gwHost), zap.Error(err))
		responseErrorD(c, errcode.Validation, nil, "unexpected models response from gateway")
		return
	}
	ResponseData(c, map[string]any{"models": ids})
}
