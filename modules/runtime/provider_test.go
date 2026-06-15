package runtime

import "testing"

func TestProviderSnapshot_QueriesAndFallback(t *testing.T) {
	snap := newProviderSnapshot([]providerDef{
		{Name: "claude", UpgradeTimeoutSec: 600, Status: "active"},
		{Name: "openclaw", UpgradeTimeoutSec: 720, Status: "active"},
		{Name: "codex", UpgradeTimeoutSec: 600, Status: "disabled"},
	})

	if !snap.IsActiveKind("claude") {
		t.Errorf("claude should be active")
	}
	if snap.IsActiveKind("codex") {
		t.Errorf("codex should NOT be active (disabled)")
	}
	if snap.IsActiveKind("nope") {
		t.Errorf("unknown kind must not be active")
	}
	if !snap.IsKnownKind("codex") {
		t.Errorf("codex is known (just disabled)")
	}
	if snap.IsKnownKind("nope") {
		t.Errorf("nope is not known")
	}
	if got := snap.TimeoutSec("openclaw"); got != 720 {
		t.Errorf("openclaw timeout = %d, want 720", got)
	}
	if got := snap.TimeoutSec("nope"); got != defaultUpgradeTimeoutSec {
		t.Errorf("unknown timeout = %d, want default %d", got, defaultUpgradeTimeoutSec)
	}
	active := snap.ActiveNames()
	if len(active) != 2 {
		t.Fatalf("active names = %v, want 2", active)
	}
}

func TestFallbackSnapshot_ActiveOnlyClaudeOpenclaw(t *testing.T) {
	snap := fallbackProviderSnapshot()
	if !snap.IsActiveKind("claude") || !snap.IsActiveKind("openclaw") {
		t.Errorf("fallback must keep claude+openclaw active")
	}
	if snap.IsActiveKind("codex") || snap.IsActiveKind("hermes") {
		t.Errorf("fallback must NOT activate codex/hermes")
	}
}
