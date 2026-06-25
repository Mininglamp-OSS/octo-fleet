package runtime

import "testing"

// TestComputeDaemonHint locks in the daemon version-source contract: the
// GET /runtimes daemon update hint is derived from the device's octo-daemon
// device_component reported_version (npm-installed), the same source the upgrade
// gate (createDaemonUpgradeTask → queryDaemonReportedVersion) uses. This
// prevents the hint and gate from diverging (PR #59 review: a hint sourced from
// reported_version while the gate read metadata.cli_version produced a
// 409-Conflict dead end).
func TestComputeDaemonHint(t *testing.T) {
	comps := func(version string) []deviceComponentView {
		if version == "" {
			return []deviceComponentView{{Name: "claude-agent-sdk", Version: "9.9.9"}}
		}
		return []deviceComponentView{
			{Name: "octo-daemon", Version: version},
			{Name: "octo-cli", Version: "1.2.3"},
		}
	}

	tests := []struct {
		name        string
		components  []deviceComponentView
		latest      string
		wantOK      bool
		wantCurrent string
	}{
		{"reported older than latest → hint", comps("0.0.4"), "0.0.5", true, "0.0.4"},
		{"reported equals latest → no hint", comps("0.0.5"), "0.0.5", false, ""},
		{"reported newer than latest → no hint", comps("0.0.6"), "0.0.5", false, ""},
		{"no octo-daemon component → no hint", comps(""), "0.0.5", false, ""},
		{"empty reported version → no hint", comps(""), "0.0.5", false, ""},
		{"no latest published → no hint", comps("0.0.4"), "", false, ""},
		{"dev reported treated as older → hint", comps("dev"), "0.0.5", true, "dev"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, ok := computeDaemonHint(tt.components, tt.latest)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if !hint.HasUpdate {
				t.Errorf("HasUpdate = false, want true")
			}
			if hint.Current != tt.wantCurrent {
				t.Errorf("Current = %q, want %q", hint.Current, tt.wantCurrent)
			}
			if hint.LatestVersion != tt.latest {
				t.Errorf("LatestVersion = %q, want %q", hint.LatestVersion, tt.latest)
			}
		})
	}
}
