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

	// v3 §4.5: explicit signal from server that ?include=context took
	// effect (server >= v2). Distinguishes "server returned empty spaces"
	// from "pre-v2 server omitted the field" — needed for fail-closed
	// X-Space-Id checks (without this, both shapes look identical after
	// Go json decode erases empty slices via omitempty).
	ContextIncluded bool `json:"context_included"`

	// v2 鉴权关系数据补全: populated by server when middleware passes
	// ?include=context. Empty/nil when caller did not opt-in or server
	// version pre-dates v2.
	Spaces           []string            `json:"spaces,omitempty"`
	OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space,omitempty"`
}

type verifyBotResp struct {
	BotUID    string `json:"bot_uid"`
	BotName   string `json:"bot_name"`
	OwnerUID  string `json:"owner_uid"`
	OwnerName string `json:"owner_name"`
	SpaceID   string `json:"space_id"`
}

// verifyAPIKeyResp mirrors POST /v1/auth/verify-api-key on server
// (合并 plan §3). The endpoint returns the user + bound space; when
// middleware passes ?include=context the response also carries owned_bots
// keyed by space (always a single-key map for api_key — it's bound to
// exactly one space).
type verifyAPIKeyResp struct {
	UID     string `json:"uid"`
	SpaceID string `json:"space_id"`
	// v3 §4.5: same signal as verifyTokenResp.
	ContextIncluded bool                `json:"context_included"`
	OwnedBots       map[string][]string `json:"owned_bots,omitempty"`
}

// AuthMiddleware authenticates requests by calling octoim's verify API.
// Supports three auth paths (合并 plan §4 三协议):
//   - User:    "token" header → POST /v1/auth/verify (session)
//   - Bot:     "Authorization: Bearer bf_<token>" → POST /v1/auth/verify-bot
//   - APIKey:  "Authorization: Bearer uk_<key>"   → POST /v1/auth/verify-api-key (daemon)
//
// Prefix dispatch is strict (HasPrefix + min length). Bearer headers
// without uk_ or bf_ prefix are rejected outright (Phase 4 收紧, 不再
// fall through to bot path letting server verify-bot reject).
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
			// Bot token: strict bf_ prefix.
			if strings.HasPrefix(token, botTokenPrefix) {
				handleBotAuth(c, client, cfg.OctoIMURL, token, cache)
				return
			}
			// 合并 plan 决策一+二 Phase 4: 非 uk_/bf_ 的 Bearer 直接 401
			// (旧 JWT 路径已删, 不再 fall through 让 server verify-bot 兜底).
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": gin.H{"code": "UNAUTHORIZED", "message": "Bearer token must start with uk_ or bf_"},
			})
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
		applyUserResult(c, result)
		return
	}

	body, _ := json.Marshal(map[string]string{"token": token})
	// ?include=context asks server for spaces + owned_bots_by_space so the
	// handler layer can enforce X-Space-Id membership + bot ownership
	// against server-validated data instead of client-supplied headers.
	resp, err := client.Post(baseURL+"/v1/auth/verify?include=context", "application/json", bytes.NewReader(body))
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

	applyUserResult(c, &result)
	cache.set("user:"+token, &result, 60*time.Second)
}

// applyUserResult injects verified user context into gin ctx. Mirrors the
// shape used by both the cache-hit and fresh-fetch paths so they stay in
// sync (subtle bugs from forgetting to update one side were the original
// cause of issue #75-class regressions).
func applyUserResult(c *gin.Context, result *verifyTokenResp) {
	c.Set("uid", result.UID)
	c.Set("name", result.Name)
	c.Set("role", result.Role)
	c.Set("auth_kind", AuthKindSession)

	relatedUIDs := []string{result.UID}
	for _, bot := range result.OwnedBots {
		relatedUIDs = append(relatedUIDs, bot.UID)
	}
	c.Set("related_uids", relatedUIDs)

	// v3 §4.5: ALWAYS set verify_context_included + spaces + owned map
	// when server confirmed v2 (ContextIncluded=true), even if spaces is
	// the empty slice. The middleware wrapper uses ContextIncluded to
	// pick fail-closed (v2) vs trust-the-header (pre-v2) for X-Space-Id
	// validation. Setting empty []string{} / map for v2-empty caller is
	// distinct from leaving the keys unset for pre-v2 server.
	if result.ContextIncluded {
		c.Set("verify_context_included", true)
		spaces := result.Spaces
		if spaces == nil {
			spaces = []string{}
		}
		c.Set("spaces", spaces)
		owned := result.OwnedBotsBySpace
		if owned == nil {
			owned = map[string][]string{}
		}
		c.Set("owned_bots_by_space", owned)
	}
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
		return
	}

	body, _ := json.Marshal(map[string]string{"api_key": apiKey})
	// ?include=context asks server for owned_bots map (single-key for the
	// api_key's bound space) so handlers can enforce per-bot ownership
	// without a separate query.
	resp, err := client.Post(baseURL+"/v1/auth/verify-api-key?include=context", "application/json", bytes.NewReader(body))
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

}

func applyAPIKeyResult(c *gin.Context, r *verifyAPIKeyResp) {
	c.Set("uid", r.UID)
	c.Set("space_id", r.SpaceID)
	c.Set("auth_kind", AuthKindAPIKey)
	// v3 §4.5: api_key path is bound to exactly one space, so the
	// X-Space-Id wrapper check doesn't apply here (space_id is already
	// set from the verify response). But we still set the flag for
	// downstream handlers that branch on it.
	if r.ContextIncluded {
		c.Set("verify_context_included", true)
		// api_key has a single bound space; surface as []string for the
		// same `spaces` ctx key shape that user path uses, so callers
		// like the matter actor_uid helpers can read uniformly.
		c.Set("spaces", []string{r.SpaceID})
		owned := r.OwnedBots
		if owned == nil {
			owned = map[string][]string{}
		}
		c.Set("owned_bots_by_space", owned)
	}
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
// session 路径下 AuthMiddleware 不注入 space_id (一 user 可在多 space),
// wrapper 末尾从 X-Space-Id header 兜底注入. 浏览器 axios interceptor 已
// 自动带这个 header; raw fetch caller 需要手动加.
//
// 注: 不要在 handle*Auth / RequireKind 末尾调 c.Next() — gin engine 的
// for-loop 会自动 advance handler 链, 显式 c.Next 会嵌套触发 business
// handler 跑在 wrapper 后续逻辑之前 (e.g. 拿不到这里 set 的 space_id).
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
		// session 路径补 space_id (api_key/bot 路径已由 AuthMiddleware 注入).
		//
		// v3 §4.5 (yujiawei P1, fail-closed): the v2 implementation gated
		// the X-Space-Id check on `len(spaces) > 0` and trusted the header
		// blindly otherwise. That collapsed two cases (v2 server returned
		// empty spaces vs pre-v2 server omitted the field) into the same
		// fail-open branch — listBots could enumerate any space's bots
		// via spoofed X-Space-Id when the caller belonged to zero spaces.
		//
		// v3 distinguishes them via verify_context_included (set true only
		// when server confirmed it spoke v2 contract):
		//   - context_included=true  → MUST validate X-Space-Id ∈ spaces;
		//                              empty spaces means caller has zero
		//                              memberships, reject any X-Space-Id
		//   - context_included=false → pre-v2 server; fallback to header
		//                              trust (defense-in-depth: listBots
		//                              SQL forces owner_uid=loginUID anyway).
		if _, exists := c.Get("space_id"); !exists {
			sid := c.GetHeader("X-Space-Id")
			if sid == "" {
				return
			}
			ctxInclVal, _ := c.Get("verify_context_included")
			ctxIncl, _ := ctxInclVal.(bool)
			if ctxIncl {
				// v2 server: hard membership check, fail-closed on empty.
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
					// Cover both: (a) X-Space-Id is not in the verified
					// set, (b) caller's verified set is empty (zero
					// memberships). Same 403, no info leak.
					c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
						"error": gin.H{
							"code":    "FORBIDDEN",
							"message": "X-Space-Id not in caller's space membership",
						},
					})
					return
				}
			}
			// pre-v2 fallback OR v2 with valid X-Space-Id → set space_id.
			// Pre-v2 path is a transitional grace window for rolling
			// upgrade; defense-in-depth lives in listBotsBySpace + sibling
			// SQL paths which force owner_uid=loginUID irrespective of
			// X-Space-Id trust.
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
		// Unknown scope at config time = crash early.
		panic("auth: unknown scope " + scope + " (want daemon|web|bot)")
	}
}

// MatchesSpace returns true when the request's authenticated space_id
// matches the passed spaceID. The middleware injects space_id from
// verify-api-key (api_key path) or verify-bot (bot path); the user/session
// path doesn't inject space_id by default, so user-facing handlers must
// either trust X-Space-Id or skip this check.
//
// SECURITY NOTE (v3 §4.2, aunknown B2): for the session path, ctx
// space_id is the X-Space-Id header — client-controlled. MatchesSpace
// compares that header against the handler's spaceID parameter (also
// from the URL query), so the call effectively asks "did the client
// claim the same space twice?" — useful for catching typos / replay
// noise, NOT for cross-space access control.
//
// Real authz on the session path comes from two layers above this:
//  1. middleware Middleware wrapper validates X-Space-Id ∈ verified
//     spaces from /v1/auth/verify?include=context, but ONLY when
//     verify_context_included is true (v2 server). Pre-v2 fallback
//     trusts the header.
//  2. handler SQL MUST enforce owner_uid=loginUID (or equivalent
//     tenant-bound predicate). v3 §4.5 (defense-in-depth C) made the
//     filter mandatory in listBotsBySpace to close the cross-space
//     enumeration that bypassed (1) during the pre-v2 fallback.
//
// In short: MatchesSpace is a sanity check, not a permission check.
// A handler that relies on MatchesSpace alone for cross-space isolation
// has a bug.
func MatchesSpace(c *wkhttp.Context, spaceID string) bool {
	v, ok := c.Get("space_id")
	if !ok {
		return false
	}
	tokenSpaceID, _ := v.(string)
	return tokenSpaceID != "" && tokenSpaceID == spaceID
}
