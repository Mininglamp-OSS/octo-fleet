package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockOctoServer mirrors matter test fixture so we can exercise the full
// wrapper → AuthMiddleware → server-verify path without spinning up a real
// octo-server.
type mockSrv struct {
	*httptest.Server
	apiKeyCalls int32
	userCalls   int32
}

func newMockSrv() *mockSrv {
	m := &mockSrv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/verify-api-key", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.apiKeyCalls, 1)
		var req struct {
			APIKey string `json:"api_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"uid":      "u_" + req.APIKey,
			"space_id": "sp_" + req.APIKey,
		})
	})
	mux.HandleFunc("/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.userCalls, 1)
		var req struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(verifyTokenResp{
			UID:  "u_" + req.Token,
			Name: "User",
			Role: "member",
		})
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// newTestWk wires a real wkhttp.WKHttp router with the fleet Middleware()
// wrapper for a given scope, plus a /probe endpoint that echoes ctx keys.
func newTestWk(srvURL, scope string) *wkhttp.WKHttp {
	gin.SetMode(gin.TestMode)
	Initialize(Config{OctoIMURL: srvURL})
	wk := wkhttp.New()
	grp := wk.Group("/v1", Middleware(scope))
	grp.GET("/probe", func(c *wkhttp.Context) {
		uid, _ := c.Get("uid")
		spaceID, _ := c.Get("space_id")
		kind, _ := c.Get("auth_kind")
		ownerUID, _ := c.Get("owner_uid")
		c.Response(gin.H{
			"uid":       uid,
			"space_id":  spaceID,
			"auth_kind": kind,
			"owner_uid": ownerUID,
		})
	})
	return wk
}

// Wrapper smoke test: api_key Bearer → middleware sets uid + space_id +
// auth_kind on the gin context, business handler can read them. This is
// the regression test for the c.Next() nesting bug — if handle*Auth /
// RequireKind call c.Next, the handler runs BEFORE this wrapper finishes
// and uid/space_id wouldn't be visible.
func TestMiddleware_APIKey_HappyPath(t *testing.T) {
	srv := newMockSrv()
	defer srv.Close()
	wk := newTestWk(srv.URL, "daemon")

	apiKey := "uk_" + strings.Repeat("a", 32)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	wk.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "u_"+apiKey, resp["uid"])
	assert.Equal(t, "sp_"+apiKey, resp["space_id"])
	assert.Equal(t, AuthKindAPIKey, resp["auth_kind"])
}

// Wrapper session path: token header + X-Space-Id → middleware sets uid
// via AuthMiddleware AND injects space_id from X-Space-Id (session route
// doesn't bind a space at issue time).
func TestMiddleware_Session_XSpaceIDFallback(t *testing.T) {
	srv := newMockSrv()
	defer srv.Close()
	wk := newTestWk(srv.URL, "web")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "sess_xxx")
	req.Header.Set("X-Space-Id", "space_explicit")
	wk.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code, "body: %s", w.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "u_sess_xxx", resp["uid"])
	assert.Equal(t, "space_explicit", resp["space_id"], "wrapper must inject from X-Space-Id when middleware didn't")
	assert.Equal(t, AuthKindSession, resp["auth_kind"])
}

// Wrong scope: session caller hits a daemon-scope group → 403.
func TestMiddleware_RequireKind_Mismatch(t *testing.T) {
	srv := newMockSrv()
	defer srv.Close()
	wk := newTestWk(srv.URL, "daemon") // require apikey

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "sess_xxx") // session, not apikey
	wk.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// scopeToKind mapping.
func TestScopeToKind(t *testing.T) {
	assert.Equal(t, AuthKindAPIKey, scopeToKind("daemon"))
	assert.Equal(t, AuthKindSession, scopeToKind("web"))
	assert.Equal(t, AuthKindBot, scopeToKind("bot"))
	assert.Panics(t, func() { scopeToKind("unknown") })
}

// MatchesSpace helper smoke test (used by handler MatchesSpace check).
func TestMatchesSpace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/m", func(c *gin.Context) {
		c.Set("space_id", "sp_a")
		// Wrap *gin.Context in *wkhttp.Context for MatchesSpace signature.
		hc := &wkhttp.Context{Context: c}
		assert.True(t, MatchesSpace(hc, "sp_a"))
		assert.False(t, MatchesSpace(hc, "sp_b"))
		assert.False(t, MatchesSpace(hc, ""))
		c.Status(200)
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/m", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, 200, w.Code)
}

// ─────────────────────────────────────────────────────────────────────
// v2 鉴权关系数据补全 — cross-space regression tests
// ─────────────────────────────────────────────────────────────────────

// newCtxMockSrv returns a mock server that responds to verify with the
// v2 spaces/owned_bots_by_space fields. Lets us exercise the wrapper's
// X-Space-Id-against-spaces check without spinning up real octo-server.
//
// v3 §4.5: ContextIncluded=true marks this as a v2-or-later server. The
// wrapper uses that flag (not the presence of Spaces field) to decide
// fail-closed vs trust-the-header.
func newCtxMockSrv(spaces []string) *mockSrv {
	m := &mockSrv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.userCalls, 1)
		// Confirm middleware passed ?include=context (else the new fields
		// would be wasted upstream work).
		if r.URL.Query().Get("include") != "context" {
			http.Error(w, "missing include=context", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(verifyTokenResp{
			UID:             "u_session",
			Name:            "User",
			Role:            "member",
			ContextIncluded: true,
			Spaces:          spaces,
		})
	})
	m.Server = httptest.NewServer(mux)
	return m
}

// CORE CROSS-SPACE TEST. Reviewer fleet#24 P1: pre-v2 a caller could
// spoof X-Space-Id: B to read SpaceB's bot inventory even when their
// session token was issued for SpaceA. After v2 the wrapper validates
// the header against server-validated spaces and rejects.
func TestMiddleware_Session_XSpaceID_RejectedWhenNotMember(t *testing.T) {
	// Mock server returns the session user as a member of space-A only.
	srv := newCtxMockSrv([]string{"space-A"})
	defer srv.Close()
	wk := newTestWk(srv.URL, "web")

	// Attacker requests with X-Space-Id pointing at space-B (not a member).
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "session_xxx")
	req.Header.Set("X-Space-Id", "space-B") // ← spoof
	wk.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"wrapper must reject X-Space-Id not in server-validated spaces; body=%s", w.Body.String())
}

// Positive control: when X-Space-Id IS in the verified spaces list the
// wrapper lets it through (regression guard for over-tightening).
func TestMiddleware_Session_XSpaceID_AcceptedWhenMember(t *testing.T) {
	srv := newCtxMockSrv([]string{"space-A", "space-C"})
	defer srv.Close()
	wk := newTestWk(srv.URL, "web")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "session_xxx")
	req.Header.Set("X-Space-Id", "space-C") // ← legit member
	wk.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "space-C", resp["space_id"], "verified space must be injected into ctx")
}

// v3 §4.5: pre-v2 server fallback (no ContextIncluded flag) — wrapper
// still trusts X-Space-Id so a rolling upgrade window doesn't hard-fail
// the entire fleet. **Defense-in-depth lives in listBotsBySpace** (v3
// made owner_uid=loginUID mandatory), so a spoofed header doesn't
// translate into cross-space data access even in the fallback branch.
// Once server >= v2 is everywhere this fallback can be removed entirely.
//
// This test was previously named `_XSpaceID_Trusted`, which read as if
// fail-open were the intended steady-state behavior. Renamed to make the
// fallback semantics explicit: the wrapper trusts the header, the
// handler SQL refuses cross-owner reads — that pair is the contract.
func TestMiddleware_Session_PreV2Server_XSpaceID_HandledByHandlerOwnerFilter(t *testing.T) {
	// Mock server returns NO context_included flag (pre-v2 server). Wrapper
	// should still set space_id from header for backward compat.
	srv := &mockSrv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/verify", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&srv.userCalls, 1)
		_ = json.NewEncoder(w).Encode(verifyTokenResp{
			UID: "u_prev2", Name: "User", Role: "member",
			// No ContextIncluded — simulating pre-v2 server.
		})
	})
	srv.Server = httptest.NewServer(mux)
	defer srv.Close()
	wk := newTestWk(srv.URL, "web")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "session_xxx")
	req.Header.Set("X-Space-Id", "space-whatever")
	wk.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code,
		"pre-v2 server response must not break: wrapper falls back to trusting header (handler SQL filters cross-owner)")
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "space-whatever", resp["space_id"])
}

// v3 §4.5 (yujiawei P1): when server CONFIRMS v2 (ContextIncluded=true)
// but the caller belongs to zero spaces, the wrapper MUST reject any
// X-Space-Id. The v2 implementation collapsed this case into the pre-v2
// fallback branch (because both shapes produced `Spaces==nil` after
// omitempty erased the empty slice) — letting a zero-space caller spoof
// X-Space-Id and enumerate any space's bots via listBots without the
// `owner_uid=` query param. v3 distinguishes them via ContextIncluded.
func TestMiddleware_Session_EmptySpaces_XSpaceIDRejected(t *testing.T) {
	srv := newCtxMockSrv([]string{}) // v2 server, caller has zero memberships
	defer srv.Close()
	wk := newTestWk(srv.URL, "web")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/v1/probe", nil)
	req.Header.Set("token", "session_xxx")
	req.Header.Set("X-Space-Id", "space-target")
	wk.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"v2 server + empty spaces must reject X-Space-Id (caller is not a member of anything); body=%s", w.Body.String())
}
