// bot_token must never appear in fleet's wire contract.
//
// Fleet stores only orchestration metadata; the bot token lives on
// octo-server and the daemon fetches it directly via
// GET /v1/bot/:uid/token. BOTH bot.provision delivery paths must omit it:
//
//   - the fetch endpoint        (toFetchResponse → botProvisionFetchResponse)
//   - the heartbeat piggyback   (buildPendingBotProvision → botProvisionCmd)
//
// F-4 (lml2468) covered only the fetch path. This net also pins the
// heartbeat path, which previously still carried bot_token and leaked the
// field into the generated OpenAPI schema (Jerry-Xin fleet#40 review).
//
// Source-level assertion (this package has no DB harness — see
// owner_regression_test.go for why).

package runtime

import (
	"strings"
	"testing"
)

func structBody(t *testing.T, src, typeName string) string {
	t.Helper()
	start := strings.Index(src, "type "+typeName+" struct {")
	if start < 0 {
		t.Fatalf("struct %s not found", typeName)
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatalf("struct %s has no closing brace", typeName)
	}
	return src[start : start+end]
}

func TestBotProvisionPayloads_NeverCarryBotToken(t *testing.T) {
	for _, ck := range []struct{ file, typ string }{
		{"model.go", "botProvisionCmd"},
		{"bot_provision_fetch.go", "botProvisionFetchResponse"},
	} {
		body := structBody(t, mustReadSource(t, ck.file), ck.typ)
		if strings.Contains(body, "bot_token") || strings.Contains(body, "BotToken") {
			t.Errorf("%s must not carry bot_token — the token stays on octo-server, "+
				"the daemon fetches it separately via GET /v1/bot/:uid/token.\n\nstruct:\n%s", ck.typ, body)
		}
	}

	// The heartbeat builder must not populate a token either — even with the
	// field gone from the struct, a stray assignment would not compile, so
	// this also guards against a future re-introduction of the field.
	builder := extractFuncBody(t, mustReadSource(t, "bot.go"), "buildPendingBotProvision")
	if strings.Contains(builder, "BotToken") {
		t.Errorf("buildPendingBotProvision must not set BotToken on the heartbeat "+
			"pending_command.\n\nbody:\n%s", builder)
	}
}
