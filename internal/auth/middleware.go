package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// Config holds auth middleware configuration.
type Config struct {
	OctoIMURL string // octoim base URL
}

// Auth kind constants — injected into gin context by AuthMiddleware so
// downstream RequireKind / handlers can disambiguate the caller type.
const (
	AuthKindSession = "session" // browser session token (header: token: <session>)
	AuthKindBot     = "bot"     // bot_token (header: Authorization: Bearer bf_<token>)
	AuthKindAPIKey  = "apikey"  // daemon api_key (header: Authorization: Bearer uk_<key>)
)

// API key / bot token prefix constants.
//
// API key prefix uk_ + ≥32 random chars (合并 plan §4 prefix 严格性).
// We require strict HasPrefix match + minimum length so a Bearer with
// "uk_" + empty string can't sneak through the middleware to land on
// the server side.
const (
	apiKeyPrefix    = "uk_"
	apiKeyMinLength = 35 // "uk_" + 32-char body
	botTokenPrefix  = "bf_"
)

// --- verify API response types ---

type ownedBot struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type verifyTokenResp struct {
	UID       string     `json:"uid"`
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	OwnedBots []ownedBot `json:"owned_bots"`
}

type verifyBotResp struct {
	BotUID    string `json:"bot_uid"`
	BotName   string `json:"bot_name"`
	OwnerUID  string `json:"owner_uid"`
	OwnerName string `json:"owner_name"`
	SpaceID   string `json:"space_id"`
}

// verifyAPIKeyResp mirrors POST /v1/auth/verify-api-key on server
// (合并 plan §3). The endpoint only returns the user + bound space —
// daemon-facing handlers derive everything else from owner_uid filtering.
type verifyAPIKeyResp struct {
	UID     string `json:"uid"`
	SpaceID string `json:"space_id"`
}

// AuthMiddleware authenticates requests by calling octoim's verify API.
// Supports three auth paths (合并 plan §4 三协议):
//   - User:    "token" header → POST /v1/auth/verify (session)
//   - Bot:     "Authorization: Bearer bf_<token>" → POST /v1/auth/verify-bot
//   - APIKey:  "Authorization: Bearer uk_<key>"   → POST /v1/auth/verify-api-key (daemon)
//
// Prefix dispatch is strict (HasPrefix + min length); Bearer headers that
// don't match uk_ fall through to bot path, letting server verify-bot
// reject them. Phase 4 will tighten this to outright 401 for non-uk_/bf_.
//
// On success, injects into gin context:
//   - "uid"            — caller identity (user uid / bot uid / api_key owner uid)
//   - "auth_kind"      — one of AuthKindSession / AuthKindBot / AuthKindAPIKey
//   - "space_id"       — bound space (api_key + bot) or set later (user)
//   - "name", "role"   — user/bot only (api_key path skips these)
//   - "related_uids"   — visibility set (user/bot only)
// verifyCache caches auth verify results to avoid calling octoim on every request.
// It bounds memory via periodic eviction of expired entries and a hard cap.
type verifyCache struct {
	mu      sync.RWMutex
	entries map[string]verifyCacheEntry
}

const verifyCacheMaxSize = 10000

type verifyCacheEntry struct {
	result   interface{}
	expireAt time.Time
}

func newVerifyCache() *verifyCache {
	c := &verifyCache{entries: make(map[string]verifyCacheEntry)}
	go c.evictLoop()
	return c
}

// evictLoop removes expired entries every 5 minutes to prevent unbounded growth.
func (c *verifyCache) evictLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for k, e := range c.entries {
			if now.After(e.expireAt) {
				delete(c.entries, k)
			}
		}
		c.mu.Unlock()
	}
}

func (c *verifyCache) get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok || time.Now().After(e.expireAt) {
		return nil, false
	}
	return e.result, true
}

func (c *verifyCache) set(key string, result interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= verifyCacheMaxSize {
		// Hard cap reached — clear all to prevent unbounded growth.
		c.entries = make(map[string]verifyCacheEntry)
	}
	c.entries[key] = verifyCacheEntry{result: result, expireAt: time.Now().Add(ttl)}
}

func AuthMiddleware(cfg Config) gin.HandlerFunc {
	client := &http.Client{Timeout: 5 * time.Second}
	cache := newVerifyCache()

	return func(c *gin.Context) {
		// Bearer auth: dispatch by prefix (api_key vs bot_token).
		if authHeader := c.GetHeader("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			// API key path: strict uk_ prefix + min length to avoid
			// "uk_" with empty body sneaking through.
			if strings.HasPrefix(token, apiKeyPrefix) && len(token) >= apiKeyMinLength {
				handleAPIKeyAuth(c, client, cfg.OctoIMURL, token, cache)
				return
			}
			// Bot token (or any other Bearer): let server verify-bot decide.
			handleBotAuth(c, client, cfg.OctoIMURL, token, cache)
			return
		}

		// User token auth
		token := c.GetHeader("token")
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"code": "UNAUTHORIZED", "message": "missing token or Authorization header"},
			})
			return
		}
		handleUserAuth(c, client, cfg.OctoIMURL, token, cache)
	}
}

func handleUserAuth(c *gin.Context, client *http.Client, baseURL, token string, cache *verifyCache) {
	// Check cache
	if cached, ok := cache.get("user:" + token); ok {
		result := cached.(*verifyTokenResp)
		c.Set("uid", result.UID)
		c.Set("name", result.Name)
		c.Set("role", result.Role)
		c.Set("auth_kind", AuthKindSession)
		relatedUIDs := []string{result.UID}
		for _, bot := range result.OwnedBots {
			relatedUIDs = append(relatedUIDs, bot.UID)
		}
		c.Set("related_uids", relatedUIDs)
		c.Next()
		return
	}

	body, _ := json.Marshal(map[string]string{"token": token})
	resp, err := client.Post(baseURL+"/v1/auth/verify", "application/json", bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{"code": "AUTH_UNAVAILABLE", "message": "failed to reach auth service"},
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"code": "UNAUTHORIZED", "message": "invalid or expired token"},
		})
		return
	}

	var result verifyTokenResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "AUTH_ERROR", "message": "failed to parse auth response"},
		})
		return
	}

	c.Set("uid", result.UID)
	c.Set("name", result.Name)
	c.Set("role", result.Role)
	c.Set("auth_kind", AuthKindSession)

	relatedUIDs := []string{result.UID}
	for _, bot := range result.OwnedBots {
		relatedUIDs = append(relatedUIDs, bot.UID)
	}
	c.Set("related_uids", relatedUIDs)

	// Cache for 60s
	cache.set("user:"+token, &result, 60*time.Second)

	c.Next()
}

func handleBotAuth(c *gin.Context, client *http.Client, baseURL, botToken string, cache *verifyCache) {
	// Check cache
	if cached, ok := cache.get("bot:" + botToken); ok {
		result := cached.(*verifyBotResp)
		c.Set("uid", result.BotUID)
		c.Set("name", result.BotName)
		c.Set("role", "bot")
		c.Set("auth_kind", AuthKindBot)
		if result.OwnerUID != "" {
			c.Set("owner_uid", result.OwnerUID)
		}
		if result.OwnerName != "" {
			c.Set("owner_name", result.OwnerName)
		}
		if result.SpaceID != "" {
			c.Set("space_id", result.SpaceID)
		}
		relatedUIDs := []string{result.BotUID}
		c.Set("related_uids", relatedUIDs)
		c.Next()
		return
	}

	body, _ := json.Marshal(map[string]string{"bot_token": botToken})
	resp, err := client.Post(baseURL+"/v1/auth/verify-bot", "application/json", bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{"code": "AUTH_UNAVAILABLE", "message": "failed to reach auth service"},
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"code": "UNAUTHORIZED", "message": "invalid bot token"},
		})
		return
	}

	var result verifyBotResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "AUTH_ERROR", "message": "failed to parse auth response"},
		})
		return
	}

	c.Set("uid", result.BotUID)
	c.Set("name", result.BotName)
	c.Set("role", "bot")
	c.Set("auth_kind", AuthKindBot)
	if result.OwnerUID != "" {
		c.Set("owner_uid", result.OwnerUID)
	}
	if result.OwnerName != "" {
		// Stored so notification handlers can render the owner's name when
		// the bot acts on behalf of its owner (LLM-mediated timeline path).
		c.Set("owner_name", result.OwnerName)
	}
	if result.SpaceID != "" {
		c.Set("space_id", result.SpaceID)
	}

	// Build related UIDs: [self] only. Owner-side visibility of bot matters
	// is handled via the user-auth path's owned_bots expansion.
	relatedUIDs := []string{result.BotUID}
	c.Set("related_uids", relatedUIDs)

	cache.set("bot:"+botToken, &result, 60*time.Second)

	c.Next()
}

// handleAPIKeyAuth verifies a daemon api_key by calling server's
// /v1/auth/verify-api-key endpoint and caches the result for 60s.
//
// Injects uid + space_id + auth_kind="apikey" only — api_key callers
// (daemon / runtime / agent processes) don't need name/role/related_uids;
// 决策二信任模型把跨 user 隔离推到业务 SQL WHERE owner_uid=?.
func handleAPIKeyAuth(c *gin.Context, client *http.Client, baseURL, apiKey string, cache *verifyCache) {
	if cached, ok := cache.get("apikey:" + apiKey); ok {
		result := cached.(*verifyAPIKeyResp)
		applyAPIKeyResult(c, result)
		c.Next()
		return
	}

	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	resp, err := client.Post(baseURL+"/v1/auth/verify-api-key", "application/json", bytes.NewReader(body))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error": gin.H{"code": "AUTH_UNAVAILABLE", "message": "failed to reach auth service"},
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": gin.H{"code": "UNAUTHORIZED", "message": "invalid api_key"},
		})
		return
	}

	var result verifyAPIKeyResp
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{"code": "AUTH_ERROR", "message": "failed to parse auth response"},
		})
		return
	}

	applyAPIKeyResult(c, &result)
	cache.set("apikey:"+apiKey, &result, 60*time.Second)

	c.Next()
}

func applyAPIKeyResult(c *gin.Context, r *verifyAPIKeyResp) {
	c.Set("uid", r.UID)
	c.Set("space_id", r.SpaceID)
	c.Set("auth_kind", AuthKindAPIKey)
}

// RequireKind enforces that the authenticated caller's auth_kind matches
// one of the allowed kinds (合并 plan §4 Endpoint 鉴权矩阵).
//
// Returns 403 (not 401 — credential is valid but the endpoint disallows
// this caller type) on mismatch. Must be mounted after AuthMiddleware:
//
//	r.Group("/api/v1/internal/bot-tasks",
//	    auth.AuthMiddleware(cfg),
//	    auth.RequireKind(auth.AuthKindAPIKey))
//
// Phase 2 ships the helper; Phase 3B is where individual endpoint groups
// adopt it as daemon-facing handlers migrate off DualAuth onto AuthMiddleware.
func RequireKind(allowed ...string) gin.HandlerFunc {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	return func(c *gin.Context) {
		raw, _ := c.Get("auth_kind")
		kind, _ := raw.(string)
		if _, ok := set[kind]; !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":    "AUTH_KIND_NOT_ALLOWED",
					"message": fmt.Sprintf("endpoint requires one of: %s", strings.Join(allowed, ", ")),
				},
			})
			return
		}
		c.Next()
	}
}

// --- fleet-specific wkhttp compatibility layer ---
//
// fleet api.go uses octo-lib's wkhttp.HandlerFunc (which wraps gin.HandlerFunc
// via wkhttp.Context embedding *gin.Context). The wrappers below let us
// drop the new AuthMiddleware in without touching every endpoint group.
//
// Old fleet auth/jwt.go exposed:
//   - Initialize(jwksURL)             → Initialize(Config)        (new)
//   - Middleware(scope) wkhttp.HF     → Middleware(scope) wkhttp.HF (same sig)
//   - MatchesSpace(c, sid)            → MatchesSpace(c, sid)        (unchanged)
//
// Scope semantics changed (was JWT scope, now auth_kind):
//   - scope "daemon" → require auth_kind=apikey (合并 plan §4 fleet 鉴权矩阵)
//   - scope "web"    → require auth_kind=session
//   - scope "bot"    → require auth_kind=bot   (new — fleet may grow bot-facing endpoints)
//
// Phase 4 will inline this — change api.go endpoint groups to use
// AuthMiddleware + RequireKind directly, then drop the wrappers.

var defaultCfg Config

// Initialize stores the config singleton used by Middleware(). Must be
// called from main before any module Route() runs.
func Initialize(cfg Config) {
	defaultCfg = cfg
}

// Middleware is the wkhttp-flavored wrapper around AuthMiddleware +
// RequireKind. Panics if Initialize wasn't called — that's a config bug.
//
// Aliases "uid" → "owner_uid" in the gin context so the existing
// handlers' c.MustGet("owner_uid") calls keep working without churn
// (合并 plan §3 字段名跨服务映射: fleet 业务列就叫 owner_uid).
func Middleware(scope string) wkhttp.HandlerFunc {
	if defaultCfg.OctoIMURL == "" {
		panic("auth: Initialize not called before Middleware")
	}
	authH := AuthMiddleware(defaultCfg)
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
		if uid, ok := c.Get("uid"); ok {
			if s, ok2 := uid.(string); ok2 && s != "" {
				c.Set("owner_uid", s)
			}
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
		// Unknown scope at config time = crash early.
		panic("auth: unknown scope " + scope + " (want daemon|web|bot)")
	}
}

// MatchesSpace returns true when the request's authenticated space_id
// matches the passed spaceID. The middleware injects space_id from
// verify-api-key (api_key path) or verify-bot (bot path); the user/session
// path doesn't inject space_id by default, so user-facing handlers must
// either trust X-Space-Id or skip this check.
func MatchesSpace(c *wkhttp.Context, spaceID string) bool {
	v, ok := c.Get("space_id")
	if !ok {
		return false
	}
	tokenSpaceID, _ := v.(string)
	return tokenSpaceID != "" && tokenSpaceID == spaceID
}
