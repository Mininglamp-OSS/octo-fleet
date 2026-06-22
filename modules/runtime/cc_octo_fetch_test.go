package runtime

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestFetchCcOctoConfig_HasOwnershipGate(t *testing.T) {
	src, err := os.ReadFile("cc_octo_fetch.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, must := range []string{
		`c.MustGet("uid")`,        // owner from api_key
		`c.MustGet("space_id")`,   // space from api_key
		`c.Query("runtime_id")`,   // self-reported runtime, must be gated
		`rt.db.queryByID(`,        // runtime ownership lookup
		`rt.ccSecrets.get(`,       // transient store lookup
		`from_version`,            // 必须选出 from_version 以区分 install vs 普通 upgrade
		`errcode.Forbidden`,       // gate rejection path
		`errcode.NotFound`,        // 普通 upgrade 无 secret → 404 (daemon 走普通 upgrade)
		`errcode.Conflict`,        // install 缺 secret → 409 (daemon report failed)
		`errcode.Gone`,            // 终态 task → 410 (stale, daemon mark done)
		`errcode.InternalError`,   // DB error must be retryable, not collapsed into NotFound
	} {
		if !strings.Contains(body, must) {
			t.Errorf("fetchCcOctoConfig missing required gate/element: %s", must)
		}
	}
	// runtime_id must be cross-checked against the task's recorded runtime_id.
	if !strings.Contains(body, "RuntimeID") {
		t.Error("fetchCcOctoConfig must verify the task's recorded runtime_id matches the caller-supplied one")
	}
	// 终态 task 不可再 fetch:必须按 status 放行 in-flight、拒终态。
	if !regexp.MustCompile(`"pending"|"dispatched"|"installing"`).MatchString(body) {
		t.Error("fetchCcOctoConfig must gate on task status (only in-flight fetchable)")
	}
}
