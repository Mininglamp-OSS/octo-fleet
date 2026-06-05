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
