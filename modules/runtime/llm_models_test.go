package runtime

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

func TestParseModelIDs(t *testing.T) {
	body := []byte(`{"data":[{"id":"ali/deepseek-r1","type":"model"},{"id":"vertexai/claude-opus-4-8"}]}`)
	ids, err := parseModelIDs(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "ali/deepseek-r1" || ids[1] != "vertexai/claude-opus-4-8" {
		t.Fatalf("got %v", ids)
	}
}

func TestParseModelIDs_Empty(t *testing.T) {
	ids, err := parseModelIDs([]byte(`{"data":[]}`))
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

func TestParseModelIDs_Garbage(t *testing.T) {
	if _, err := parseModelIDs([]byte(`not json`)); err == nil {
		t.Fatal("expected error on non-json body")
	}
}

func TestIsUnsafeIP(t *testing.T) {
	unsafe := []string{
		"127.0.0.1", "::1", // loopback
		"10.0.0.5", "192.168.1.1", "172.16.0.1", "fd00::1", // private
		"169.254.1.1", "fe80::1", // link-local
		"0.0.0.0", "::", // unspecified
		"::ffff:10.0.0.1", // v4-mapped private
		"64:ff9b::a00:1",  // NAT64-embedded 10.0.0.1
	}
	for _, s := range unsafe {
		if !isUnsafeIP(net.ParseIP(s)) {
			t.Errorf("expected unsafe: %s", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111"} {
		if isUnsafeIP(net.ParseIP(s)) {
			t.Errorf("expected safe: %s", s)
		}
	}
	if !isUnsafeIP(nil) {
		t.Error("nil must be unsafe")
	}
}

func TestValidateProxyURL(t *testing.T) {
	for _, bad := range []string{"http://gw.test/v1", "https://localhost/v1", "not a url", ""} {
		if err := validateProxyURL(bad); err == nil {
			t.Errorf("expected reject: %q", bad)
		}
	}
	if err := validateProxyURL("https://gw.example.com/v1"); err != nil {
		t.Errorf("expected allow: %v", err)
	}
}

func TestFetchLLMModels_ProxiesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()

	// Inject the test server's client (trusts its self-signed cert and bypasses
	// the dial-time SSRF gate that would reject 127.0.0.1 in production).
	rt := &Runtime{proxyClient: srv.Client(), Log: log.NewTLog("test")}

	w := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(w)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/runtimes/llm-models",
		strings.NewReader(`{"gateway_url":"`+srv.URL+`","api_key":"sk"}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	rt.fetchLLMModels(&wkhttp.Context{Context: ginCtx})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"m1"`) || !strings.Contains(w.Body.String(), `"m2"`) {
		t.Fatalf("models missing in body: %s", w.Body.String())
	}
}

func TestFetchLLMModels_RejectsNonHTTPS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rt := &Runtime{Log: log.NewTLog("test")}
	w := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(w)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/runtimes/llm-models",
		strings.NewReader(`{"gateway_url":"http://10.0.0.1/v1","api_key":"sk"}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	rt.fetchLLMModels(&wkhttp.Context{Context: ginCtx})
	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 for http gateway, got 200: %s", w.Body.String())
	}
}
