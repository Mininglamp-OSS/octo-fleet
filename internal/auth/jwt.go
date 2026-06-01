// Package auth implements JWT verification for octo-fleet.
//
// fleet is a JWT verifier, not an issuer — it fetches octo-server's
// public key (JWKS) once at startup and verifies bearer tokens locally.
// On signature failure or unknown kid, the cache is refreshed once
// before giving up (handles key rotation without restart).
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/go-jose/go-jose/v3"
	"github.com/go-jose/go-jose/v3/jwt"
	"go.uber.org/zap"
)

// Claims mirrors octo-server's auth_jwt.Claims exactly. Keeping them
// in lock-step is a service-contract; any new claim added on the issuer
// side must be added here too.
type Claims struct {
	jwt.Claims
	Scope    string `json:"scope"`
	SpaceID  string `json:"space_id,omitempty"`
	DaemonID string `json:"daemon_id,omitempty"`
}

// Verifier holds the cached JWKS and refreshes it on demand. Safe for
// concurrent use. A single instance is shared across all middleware
// invocations in the process.
type Verifier struct {
	jwksURL string
	httpc   *http.Client
	log.Log

	mu       sync.RWMutex
	keys     map[string]*jose.JSONWebKey
	lastFetch time.Time
	// minRefreshInterval bounds key-rotation refreshes — without it a
	// flood of bad tokens could DDoS the issuer.
	minRefreshInterval time.Duration
}

// NewVerifier builds a Verifier and eagerly loads the JWKS. Returns
// the verifier with whatever keys it managed to fetch; if the initial
// fetch fails, callers still get a working object that will retry on
// the first request.
func NewVerifier(jwksURL string) *Verifier {
	v := &Verifier{
		jwksURL:            jwksURL,
		httpc:              &http.Client{Timeout: 5 * time.Second},
		keys:               map[string]*jose.JSONWebKey{},
		minRefreshInterval: 10 * time.Second,
		Log:                log.NewTLog("AuthJWT-Verifier"),
	}
	if err := v.refresh(); err != nil {
		v.Warn("initial JWKS fetch failed (will retry on first request)",
			zap.Error(err), zap.String("url", jwksURL))
	}
	return v
}

// refresh pulls the JWKS document and replaces the in-memory cache.
// Errors leave the existing cache intact.
func (v *Verifier) refresh() error {
	req, err := http.NewRequest(http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	if len(set.Keys) == 0 {
		return errors.New("JWKS empty")
	}
	keys := make(map[string]*jose.JSONWebKey, len(set.Keys))
	for i := range set.Keys {
		k := &set.Keys[i]
		keys[k.KeyID] = k
	}
	v.mu.Lock()
	v.keys = keys
	v.lastFetch = time.Now()
	v.mu.Unlock()
	v.Info("JWKS refreshed", zap.Int("key_count", len(keys)))
	return nil
}

// keyForKid returns the cached key for kid, optionally refreshing the
// JWKS once if the kid is missing and the last refresh wasn't too recent.
func (v *Verifier) keyForKid(kid string) (*jose.JSONWebKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	since := time.Since(v.lastFetch)
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	if since < v.minRefreshInterval {
		return nil, fmt.Errorf("unknown kid %q (cache still fresh)", kid)
	}
	if err := v.refresh(); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q after refresh", kid)
}

// Verify parses and validates a JWT, returning the claims on success.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parsed, err := jwt.ParseSigned(token)
	if err != nil {
		return nil, fmt.Errorf("parse jwt: %w", err)
	}
	if len(parsed.Headers) == 0 {
		return nil, errors.New("jwt missing headers")
	}
	kid := parsed.Headers[0].KeyID
	if kid == "" {
		return nil, errors.New("jwt missing kid")
	}
	key, err := v.keyForKid(kid)
	if err != nil {
		return nil, err
	}
	var cl Claims
	if err := parsed.Claims(key.Key, &cl); err != nil {
		return nil, fmt.Errorf("verify claims: %w", err)
	}
	if err := cl.Validate(jwt.Expected{Time: time.Now()}); err != nil {
		return nil, fmt.Errorf("claims expired/invalid: %w", err)
	}
	return &cl, nil
}

// defaultVerifier is the process-wide singleton. Modules call Middleware
// without threading the verifier through every constructor.
var defaultVerifier *Verifier

// Initialize sets up the singleton — must be called from main before
// any module's Route() runs (where Middleware is wired into gin groups).
func Initialize(jwksURL string) {
	defaultVerifier = NewVerifier(jwksURL)
}

// Middleware is a package-level convenience for the singleton. Panics if
// Initialize wasn't called — that's a configuration bug worth crashing on.
func Middleware(requireScope string) wkhttp.HandlerFunc {
	if defaultVerifier == nil {
		panic("auth: Initialize not called before Middleware")
	}
	return defaultVerifier.Middleware(requireScope)
}

// Middleware returns a gin handler that enforces JWT auth. On success it
// sets the canonical gin context keys ("uid", "space_id", "daemon_id",
// "scope") so downstream handlers don't need to know about JWT.
//
// requireScope, when non-empty, additionally enforces claims.scope ==
// requireScope. Use "web" for browser routes, "daemon" for daemon routes,
// "" for routes accepting either.
func (v *Verifier) Middleware(requireScope string) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "missing Bearer token"})
			return
		}
		tok := strings.TrimPrefix(auth, "Bearer ")
		cl, err := v.Verify(tok)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": err.Error()})
			return
		}
		if requireScope != "" && cl.Scope != requireScope {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"msg": fmt.Sprintf("scope %q required (got %q)", requireScope, cl.Scope),
			})
			return
		}
		c.Set("uid", cl.Subject)
		// Many existing daemon handlers (copied verbatim from
		// octo-server/modules/runtime) read "owner_uid" — set both
		// keys so we don't have to fork those handlers.
		c.Set("owner_uid", cl.Subject)
		c.Set("space_id", cl.SpaceID)
		c.Set("daemon_id", cl.DaemonID)
		c.Set("scope", cl.Scope)
		// Compat with octo-lib AuthMiddleware which also sets "name"
		// (used by GetLoginName). JWT has no name; uid placeholder so
		// existing handlers don't NPE.
		c.Set("name", cl.Subject)
		c.Next()
	}
}
