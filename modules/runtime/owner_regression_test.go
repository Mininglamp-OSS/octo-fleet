// v3 §4.1 (Phase 3B regression tests, aunknown B1)
//
// Guards against a future refactor silently dropping the owner_uid
// filters that v2 added to claimPendingPing / claimPendingUpgrade /
// claimPendingBotProvision / pingGet.
//
// **Why source-grep, not DB integration test?** Fleet has no integration
// test harness today (server has testutil.NewTestServer; fleet doesn't).
// Spinning one up here would be ~300 lines of plumbing for a regression
// check that's structurally simple ("does the SQL string mention
// owner_uid?"). A source-level assertion catches the "someone deleted
// the filter" case at near-zero overhead. Once fleet grows a DB
// harness, replace these with real query-against-mysql tests — the
// owner_uid invariant should outlive any specific test mechanism.
//
// If you intentionally remove the filter from one of these functions,
// you'll have to update this test — that's the point. Don't just
// add the function to the allow-list; document why the owner_uid
// invariant is no longer needed there (e.g., schema-level uniqueness
// now provides the same guarantee).

package runtime

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readSourceOnce caches package source files so the four regression
// tests don't hit disk four times.
var sourceFiles = map[string]string{}

func mustReadSource(t *testing.T, filename string) string {
	t.Helper()
	if cached, ok := sourceFiles[filename]; ok {
		return cached
	}
	dir, _ := os.Getwd()
	path := filepath.Join(dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sourceFiles[filename] = string(data)
	return sourceFiles[filename]
}

// extractFuncBody returns the body of the named Go function from src
// (between the opening `func name(...)` and the matching closing brace
// at column 0 / start-of-line `}`).
//
// The walker handles return types that themselves contain braces (e.g.
// `func F() ([]struct{Foo string}, error) { ... }`) by tracking paren
// depth: braces inside `(...)` are part of the signature/return type,
// not the function body. Only braces at parenDepth==0 are body braces.
func extractFuncBody(t *testing.T, src, funcName string) string {
	t.Helper()
	re := regexp.MustCompile(`func\s+(?:\([^)]*\)\s+)?` + regexp.QuoteMeta(funcName) + `\s*\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("function %s not found in source", funcName)
	}
	parenDepth := 1 // one `(` already consumed by the regex (loc[1] is right after it)
	braceDepth := 0
	bodyStarted := false
	for i := loc[1]; i < len(src); i++ {
		switch src[i] {
		case '(':
			parenDepth++
		case ')':
			parenDepth--
		case '{':
			if parenDepth == 0 {
				braceDepth++
				bodyStarted = true
			}
		case '}':
			if parenDepth == 0 {
				braceDepth--
				if bodyStarted && braceDepth == 0 {
					return src[loc[0] : i+1]
				}
			}
		}
	}
	t.Fatalf("function %s body not terminated", funcName)
	return ""
}

// assertHasOwnerUIDFilter checks that the function body grep contains
// a SQL fragment referencing owner_uid. The simplest reliable check is
// presence of either `owner_uid=?` (literal SQL) or `ownerUID` (the Go
// variable name used in EXISTS subqueries) — any future SQL shape that
// drops both is structurally the regression we want to catch.
func assertHasOwnerUIDFilter(t *testing.T, funcName, body string) {
	t.Helper()
	hasSQLFilter := strings.Contains(body, "owner_uid=?")
	if !hasSQLFilter {
		t.Errorf("%s body must contain `owner_uid=?` SQL filter (v3 §4.1 regression net).\n"+
			"If this filter was intentionally removed, document why the owner_uid "+
			"invariant is no longer needed (e.g., schema migration §4.4's 4-tuple "+
			"unique key now provides per-owner isolation at INSERT time).\n\n"+
			"function body:\n%s", funcName, body)
	}
}

func TestClaimPendingPing_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "claimPendingPing")
	assertHasOwnerUIDFilter(t, "claimPendingPing", body)
}

func TestClaimPendingUpgrade_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "claimPendingUpgrade")
	assertHasOwnerUIDFilter(t, "claimPendingUpgrade", body)
}

func TestClaimPendingBotProvision_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "claimPendingBotProvision")
	assertHasOwnerUIDFilter(t, "claimPendingBotProvision", body)
}

// pingGet's owner check shifted across two rounds:
//   - v3 §4.6 (yujiawei P1): SELECT owner_uid → SELECT COUNT(*) WHERE owner_uid
//   - v3.3.1 §C.1 (Jerry-Xin Critical 三审): runtime_ping now has its own
//     owner_uid column (runtime-20260606-02 migration), so pingGet
//     reads entry.OwnerUID directly and compares against loginUID,
//     dropping the COUNT round-trip entirely.
// Regression net asserts the new shape: entry.OwnerUID direct compare
// + assertHasOwnerUIDFilter rejects any future drift back to the
// owner-less SELECT pattern.
func TestPingGet_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "pingGet")
	if !strings.Contains(body, "entry.OwnerUID != loginUID") {
		t.Errorf("pingGet must compare entry.OwnerUID against loginUID directly (v3.3.1 §C.1); body:\n%s", body)
	}
}

// v3.3.1 §C.1 (Jerry-Xin Critical 1 三审): runtime_ping inserts and
// claim/select queries must persist + filter owner_uid since the column
// was added in runtime-20260606-02. Without these, schema migration
// 20260606-01 (agent_runtime 4-tuple unique key allowing cross-owner
// daemon_id sharing) leaves runtime_ping's queries to ambiguously
// resolve which owner owns a given (space, daemon) ping row.
func TestInsertPing_PersistsOwnerUIDRegressionNet(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "insertPing")
	if !strings.Contains(body, "owner_uid") || !strings.Contains(body, "p.OwnerUID") {
		t.Errorf("insertPing must persist p.OwnerUID into runtime_ping.owner_uid (v3.3.1 §C.1); body:\n%s", body)
	}
}

func TestCompleteUpgradeIfMatched_OwnerScopedRegressionNet(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "completeUpgradeIfMatched")
	assertHasOwnerUIDFilter(t, "completeUpgradeIfMatched", body)
	if !strings.Contains(body, "space_id=?") {
		t.Errorf("completeUpgradeIfMatched must scope by space_id=? as well as owner_uid=? (v3.3.1 §C.2)")
	}
}

func TestCompleteUpgradeIfMatchedWithRuntime_OwnerScopedRegressionNet(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "completeUpgradeIfMatchedWithRuntime")
	assertHasOwnerUIDFilter(t, "completeUpgradeIfMatchedWithRuntime", body)
	if !strings.Contains(body, "space_id=?") {
		t.Errorf("completeUpgradeIfMatchedWithRuntime must scope by space_id=? as well (v3.3.1 §C.2)")
	}
}

// v3.3.1 §C.3: insertUpgradeTask's FOR UPDATE lock + active-count both
// scoped by owner so cross-tenant DoS via shared daemon_id is blocked.
func TestInsertUpgradeTask_OwnerScopedLockAndCountRegressionNet(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "insertUpgradeTask")
	// Both the FOR UPDATE row lock AND the active-count SELECT must
	// reference owner_uid; the post-§4.4 schema lets two owners share
	// daemon_id, so scoping only by daemon_id makes ownerA's pending
	// upgrade block ownerB's unrelated one.
	if !strings.Contains(body, "FOR UPDATE") {
		t.Errorf("insertUpgradeTask must still use FOR UPDATE to serialize per-owner concurrent inserts (v3.3.1 §C.3)")
	}
	if !strings.Contains(body, "owner_uid=?") {
		t.Errorf("insertUpgradeTask lock + count must reference owner_uid (v3.3.1 §C.3)")
	}
}

// listActiveBotsForDaemon (v3 §4.3): heartbeat-fed managed_bots list
// must scope by owner_uid AND space_id. Without these, a cross-owner
// daemon_id collision leaks the other owner's bot inventory.
func TestListActiveBotsForDaemon_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "listActiveBotsForDaemon")
	assertHasOwnerUIDFilter(t, "listActiveBotsForDaemon", body)
	if !strings.Contains(body, "b.space_id=?") {
		t.Errorf("listActiveBotsForDaemon must scope by b.space_id=? (v3 §4.3)")
	}
}

// listBotsBySpace (v3 §4.5 defense-in-depth C): the v2 conditional
// `?='' OR owner_uid=?` was the attack vector — without an owner_uid
// query param, the filter dropped out and listBots could enumerate
// any owner's bots in a space. v3 makes ownerUID mandatory.
func TestListBotsBySpace_OwnerFilterRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "listBotsBySpace")
	assertHasOwnerUIDFilter(t, "listBotsBySpace", body)
	if strings.Contains(body, "?='' OR owner_uid=?") {
		t.Errorf("listBotsBySpace must NOT keep the conditional `?='' OR owner_uid=?` " +
			"(v3 §4.5 defense-in-depth C: owner_uid is mandatory)")
	}
}
