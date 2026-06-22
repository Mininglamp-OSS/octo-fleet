// Package auth is fleet's auth middleware. After PR-C2-followup it
// delegates the verify HTTP call + cache to
// github.com/Mininglamp-OSS/octo-auth/sdk-go but does NOT use the
// SDK's gin Middleware wrapper — fleet's Middleware(scope) is itself
// a wkhttp wrapper that gates subsequent steps (RequireKind + v3 §4.5
// X-Space-Id fail-closed), and gin's c.Next() semantics conflict with
// wrapper-inside-wrapper composition.
//
// Instead this adapter calls Client.VerifyUser / VerifyBot /
// VerifyAPIKey directly and writes fleet's errcode envelope on
// failure. The benefit of SDK adoption is preserved (SHA-256-keyed
// LRU cache, anti-enumeration error mapping, fail-closed-on-5xx);
// the wrapper composition cost is avoided.
package auth

import (
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"

	octoauth "github.com/Mininglamp-OSS/octo-auth/sdk-go/auth"
	"github.com/Mininglamp-OSS/octo-fleet/internal/envelope"
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
)

// Config holds auth middleware configuration.
type Config struct {
	OctoIMURL string // octo-server (formerly octoim) base URL — passed to the SDK as ServerURL
}

// Auth kind constants — injected into gin context under "auth_kind" by
// AuthMiddleware so RequireKind / downstream handlers can disambiguate
// the caller type.
const (
	AuthKindSession = "session" // browser session token (header: token: <session>)
	AuthKindBot     = "bot"     // bot_token (header: Authorization: Bearer bf_<token>)
	AuthKindAPIKey  = "apikey"  // daemon api_key (header: Authorization: Bearer uk_<key>)
)

// Token-prefix constants matching the SDK's classification rules.
const (
	apiKeyPrefix    = "uk_"
	apiKeyMinLength = 35 // uk_ + 32 chars
	botTokenPrefix  = "bf_"
)

// abortErr writes a fleet-style errcode envelope. The envelope shape
// is `{"error":{"code":..., "message":...}}` per the project R2
// contract (Jerry-Xin / mochashanyao P0 review on #50: the pre-fix
// flat `{"code":..., "message":...}` shape regressed every
// authenticated route's error response and would have broken every
// client that branches on `error.code` instead of the top-level
// fields).
func abortErr(c *gin.Context, code errcode.Code) {
	c.AbortWithStatusJSON(code.HTTPStatus, envelope.Error{Error: envelope.ErrorBody{
		Code:    code.Code,
		Message: code.Message,
	}})
}

// --- SDK client singleton ---

var (
	sdkClientMu sync.Mutex
	sdkClients  = map[string]*octoauth.Client{}
)

func getOrInitSDKClient(serverURL string) *octoauth.Client {
	sdkClientMu.Lock()
	defer sdkClientMu.Unlock()
	if c, ok := sdkClients[serverURL]; ok {
		return c
	}
	c, err := octoauth.New(octoauth.Options{ServerURL: serverURL})
	if err != nil {
		c, _ = octoauth.New(octoauth.Options{ServerURL: "http://invalid"})
	}
	sdkClients[serverURL] = c
	return c
}

// AuthMiddleware extracts the token, classifies its kind from the
// prefix, calls the SDK's typed verify method, and populates fleet's
// legacy ctx keys. On any failure it writes the fleet errcode envelope
// and aborts.
//
// Token routing:
//   - Authorization: Bearer uk_... → API Key path (Daemon scope)
//   - Authorization: Bearer bf_... → Bot path
//   - Authorization: Bearer <other> → Session path
//   - token header → Session path (legacy browser fallback)
func AuthMiddleware(cfg Config) gin.HandlerFunc {
	client := getOrInitSDKClient(cfg.OctoIMURL)

	return func(c *gin.Context) {
		token, kind := extractToken(c)
		if token == "" {
			abortErr(c, errcode.AuthRequired)
			return
		}
		switch kind {
		case AuthKindAPIKey:
			handleAPIKey(c, client, token)
		case AuthKindBot:
			handleBot(c, client, token)
		case AuthKindSession:
			handleSession(c, client, token)
		default:
			abortErr(c, errcode.AuthRequired)
		}
	}
}

// extractToken returns (token, kind). kind is "" if there is no
// recognisable token at all.
func extractToken(c *gin.Context) (string, string) {
	if h := c.GetHeader("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if tok == "" {
			return "", ""
		}
		switch {
		case strings.HasPrefix(tok, apiKeyPrefix):
			if len(tok) < apiKeyMinLength {
				return "", "" // malformed
			}
			return tok, AuthKindAPIKey
		case strings.HasPrefix(tok, botTokenPrefix):
			return tok, AuthKindBot
		default:
			return tok, AuthKindSession
		}
	}
	if tok := strings.TrimSpace(c.GetHeader("token")); tok != "" {
		return tok, AuthKindSession
	}
	return "", ""
}

func handleSession(c *gin.Context, client *octoauth.Client, token string) {
	resp, err := client.VerifyUser(c.Request.Context(), token, true /* includeContext */)
	if err != nil {
		abortErr(c, mapSDKErr(err))
		return
	}
	c.Set("uid", resp.UID)
	c.Set("name", resp.Name)
	c.Set("role", resp.Role)
	c.Set("auth_kind", AuthKindSession)
	if resp.ContextIncluded {
		c.Set("verify_context_included", true)
		c.Set("spaces", resp.Spaces)
		if resp.OwnedBotsBySpace != nil {
			c.Set("owned_bots_by_space", resp.OwnedBotsBySpace)
		}
	}
	related := []string{resp.UID}
	for _, b := range resp.OwnedBots {
		related = append(related, b.UID)
	}
	c.Set("related_uids", related)
}

func handleBot(c *gin.Context, client *octoauth.Client, token string) {
	resp, err := client.VerifyBot(c.Request.Context(), token)
	if err != nil {
		abortErr(c, mapSDKErr(err))
		return
	}
	c.Set("uid", resp.BotUID)
	c.Set("name", resp.BotName)
	c.Set("role", AuthKindBot)
	c.Set("auth_kind", AuthKindBot)
	if resp.OwnerUID != "" {
		c.Set("owner_uid", resp.OwnerUID)
	}
	if resp.SpaceID != "" {
		c.Set("space_id", resp.SpaceID)
	}
	related := []string{resp.BotUID}
	if resp.OwnerUID != "" {
		related = append(related, resp.OwnerUID)
	}
	c.Set("related_uids", related)
}

func handleAPIKey(c *gin.Context, client *octoauth.Client, key string) {
	resp, err := client.VerifyAPIKey(c.Request.Context(), key, true /* includeContext */)
	if err != nil {
		abortErr(c, mapSDKErr(err))
		return
	}
	c.Set("uid", resp.UID)
	c.Set("auth_kind", AuthKindAPIKey)
	if resp.SpaceID != "" {
		c.Set("space_id", resp.SpaceID)
	}
	if resp.ContextIncluded {
		c.Set("verify_context_included", true)
		if resp.OwnedBotsBySpace != nil {
			c.Set("owned_bots_by_space", resp.OwnedBotsBySpace)
		}
	}
}

// mapSDKErr translates an octoauth sentinel error to the fleet errcode
// that should be sent back to the client.
func mapSDKErr(err error) errcode.Code {
	switch {
	case errors.Is(err, octoauth.ErrTokenMissing), errors.Is(err, octoauth.ErrTokenInvalid):
		return errcode.AuthRequired
	case errors.Is(err, octoauth.ErrBotUnavailable):
		return errcode.UpstreamUnavailable
	case errors.Is(err, octoauth.ErrUpstreamUnavailable):
		return errcode.UpstreamUnavailable
	case errors.Is(err, octoauth.ErrSpaceForbidden), errors.Is(err, octoauth.ErrKindMismatch):
		return errcode.Forbidden
	default:
		return errcode.AuthRequired
	}
}

// RequireKind aborts the request with 403 if the verified caller's
// auth_kind is not in the allowed set.
func RequireKind(allowed ...string) gin.HandlerFunc {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = struct{}{}
	}
	return func(c *gin.Context) {
		got, ok := c.Get("auth_kind")
		if !ok {
			abortErr(c, errcode.AuthRequired)
			return
		}
		gotStr, _ := got.(string)
		if _, ok := allowedSet[gotStr]; !ok {
			abortErr(c, errcode.Forbidden)
			return
		}
	}
}

// --- wkhttp wrapper ---

var (
	defaultCfgMu sync.RWMutex
	defaultCfg   Config
)

// Initialize sets the package-level default Config used by Middleware().
func Initialize(cfg Config) {
	defaultCfgMu.Lock()
	defaultCfg = cfg
	defaultCfgMu.Unlock()
}

// Middleware combines AuthMiddleware + RequireKind + v3 §4.5
// X-Space-Id fail-closed gate into one wkhttp.HandlerFunc.
// Panics if Initialize wasn't called.
//
// The wrapper does NOT call c.Next() itself — gin's engine handles
// chain progression. Each inner step (auth → require-kind → space)
// just runs sequentially within this wrapper's body; if any aborts
// (sets c.IsAborted via c.AbortWithStatusJSON), subsequent steps and
// the downstream handler are skipped automatically.
func Middleware(scope string) wkhttp.HandlerFunc {
	defaultCfgMu.RLock()
	cfg := defaultCfg
	defaultCfgMu.RUnlock()
	if cfg.OctoIMURL == "" {
		panic("auth: Initialize not called before Middleware")
	}
	authH := AuthMiddleware(cfg)
	kindH := RequireKind(scopeToKind(scope))
	return func(c *wkhttp.Context) {
		authH(c.Context)
		if c.IsAborted() {
			return
		}
		kindH(c.Context)
		if c.IsAborted() {
			return
		}
		// Session path: X-Space-Id fail-closed + fallback.
		// API-key / bot paths: AuthMiddleware already injected space_id
		// (when present), so the gate below short-circuits.
		if _, exists := c.Get("space_id"); !exists {
			sid := c.GetHeader("X-Space-Id")
			if sid == "" {
				return
			}
			ctxInclVal, _ := c.Get("verify_context_included")
			ctxIncl, _ := ctxInclVal.(bool)
			if ctxIncl {
				spacesRaw, _ := c.Get("spaces")
				spaces, _ := spacesRaw.([]string)
				allowed := false
				for _, s := range spaces {
					if s == sid {
						allowed = true
						break
					}
				}
				if !allowed {
					abortErr(c.Context, errcode.Forbidden)
					return
				}
			}
			c.Set("space_id", sid)
		}
	}
}

func scopeToKind(scope string) string {
	switch scope {
	case "daemon":
		return AuthKindAPIKey
	case "web":
		return AuthKindSession
	case "bot":
		return AuthKindBot
	default:
		panic("auth: unknown scope " + scope + " (want daemon|web|bot)")
	}
}

// MatchesSpace returns true when the request's authenticated space_id
// matches the passed spaceID.
func MatchesSpace(c *wkhttp.Context, spaceID string) bool {
	if spaceID == "" {
		return false
	}
	v, _ := c.Get("space_id")
	got, _ := v.(string)
	return got == spaceID
}

// Unused-symbol guards (errcode + http kept reachable for future
// translations of additional SDK sentinels).
var _ = http.StatusUnauthorized
