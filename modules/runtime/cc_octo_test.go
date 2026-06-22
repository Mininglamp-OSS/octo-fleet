package runtime

import "testing"

func TestExpectedPluginComponent(t *testing.T) {
	cases := []struct {
		provider string
		want     string
		ok       bool
	}{
		{"openclaw", "octo", true},
		{"claude", "cc-octo", true},
		{"codex", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := expectedPluginComponent(tc.provider)
		if got != tc.want || ok != tc.ok {
			t.Errorf("expectedPluginComponent(%q) = (%q,%v), want (%q,%v)", tc.provider, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsPluginComponent(t *testing.T) {
	for _, c := range []string{"octo", "cc-octo"} {
		if !isPluginComponent(c) {
			t.Errorf("isPluginComponent(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"octo-daemon", "claude", "openclaw", ""} {
		if isPluginComponent(c) {
			t.Errorf("isPluginComponent(%q) = true, want false", c)
		}
	}
}

func TestValidPluginForProvider(t *testing.T) {
	cases := []struct {
		component, provider string
		want                bool
	}{
		{"octo", "openclaw", true},
		{"cc-octo", "claude", true},
		{"cc-octo", "openclaw", false}, // wrong pairing
		{"octo", "claude", false},      // wrong pairing
		{"octo", "codex", false},       // unknown provider
	}
	for _, tc := range cases {
		if got := validPluginForProvider(tc.component, tc.provider); got != tc.want {
			t.Errorf("validPluginForProvider(%q,%q) = %v, want %v", tc.component, tc.provider, got, tc.want)
		}
	}
}

func TestPluginInstalledInMeta(t *testing.T) {
	cases := []struct {
		name     string
		meta     string
		plugin   string
		expected bool
	}{
		{"cc-octo installed", `{"plugins":[{"name":"cc-octo","version":"1.0.0"}]}`, "cc-octo", true},
		{"octo installed", `{"plugins":[{"name":"octo","version":"0.6.1"}]}`, "octo", true},
		{"cc-octo not installed", `{"plugins":[{"name":"octo","version":"0.6.1"}]}`, "cc-octo", false},
		{"octo not installed", `{"plugins":[{"name":"cc-octo","version":"1.0.0"}]}`, "octo", false},
		{"empty plugins", `{"plugins":[]}`, "cc-octo", false},
		{"no plugins key", `{}`, "cc-octo", false},
		{"invalid json", `not json`, "cc-octo", false},
		{"empty metadata", "", "cc-octo", false},
	}
	for _, tc := range cases {
		if got := pluginInstalledInMeta(tc.meta, tc.plugin); got != tc.expected {
			t.Errorf("pluginInstalledInMeta(%q, %q) = %v, want %v", tc.meta, tc.plugin, got, tc.expected)
		}
	}
}

func TestComputePluginHint_CcOctoHasUpdate(t *testing.T) {
	meta := `{"plugins":[{"name":"cc-octo","version":"1.0.0"}]}`
	has, ver := computePluginHint("claude", meta, map[string]string{"cc-octo": "1.1.0"})
	if !has || ver != "1.1.0" {
		t.Errorf("got (%v,%q), want (true,1.1.0)", has, ver)
	}
}

func TestComputePluginHint_OpenclawOctoNoRegression(t *testing.T) {
	meta := `{"plugins":[{"name":"octo","version":"0.6.1"}]}`
	has, ver := computePluginHint("openclaw", meta, map[string]string{"octo": "0.7.0"})
	if !has || ver != "0.7.0" {
		t.Errorf("openclaw octo hint got (%v,%q), want (true,0.7.0)", has, ver)
	}
}

func TestComputePluginHint_UpToDate(t *testing.T) {
	meta := `{"plugins":[{"name":"cc-octo","version":"1.1.0"}]}`
	if has, _ := computePluginHint("claude", meta, map[string]string{"cc-octo": "1.1.0"}); has {
		t.Error("equal versions must not report an update")
	}
}

func TestComputePluginHint_NoPluginOrNoLatest(t *testing.T) {
	// cc-octo not reported in metadata
	if has, _ := computePluginHint("claude", `{"plugins":[{"name":"octo","version":"1.0.0"}]}`, map[string]string{"cc-octo": "1.1.0"}); has {
		t.Error("wrong-named plugin must not match cc-octo")
	}
	// no latest configured
	if has, _ := computePluginHint("claude", `{"plugins":[{"name":"cc-octo","version":"1.0.0"}]}`, map[string]string{}); has {
		t.Error("missing latest version must not report an update")
	}
	// unknown provider
	if has, _ := computePluginHint("codex", `{"plugins":[]}`, map[string]string{"octo": "1.0.0"}); has {
		t.Error("unknown provider must not report a plugin update")
	}
}

// TestInstallVersionHint_ClaudeNotInstalled tests that a claude runtime with
// no cc-octo plugin + latestVersions{"cc-octo":"1.1.0"} advertises install version.
func TestInstallVersionHint_ClaudeNotInstalled(t *testing.T) {
	// Claude runtime without cc-octo installed
	meta := `{"cli_version":"0.3.0"}`
	latestVersions := map[string]string{"cc-octo": "1.1.0"}

	comp, ok := expectedPluginComponent("claude")
	if !ok || comp != componentCcOcto {
		t.Fatalf("expected cc-octo for claude, got %q (ok=%v)", comp, ok)
	}

	installed := pluginInstalledInMeta(meta, comp)
	if installed {
		t.Fatal("expected cc-octo NOT installed")
	}

	installVer := latestVersions[comp]
	if installVer == "" {
		t.Fatal("expected installable version to exist")
	}

	if installVer != "1.1.0" {
		t.Errorf("got install version %q, want 1.1.0", installVer)
	}
}

// TestInstallVersionHint_ClaudeInstalled tests that a claude runtime WITH
// cc-octo installed does NOT advertise PluginInstallVersion.
func TestInstallVersionHint_ClaudeInstalled(t *testing.T) {
	// Claude runtime with cc-octo already installed
	meta := `{"cli_version":"0.3.0","plugins":[{"name":"cc-octo","version":"1.0.0"}]}`
	latestVersions := map[string]string{"cc-octo": "1.1.0"}

	comp, ok := expectedPluginComponent("claude")
	if !ok {
		t.Fatal("expected cc-octo for claude")
	}

	installed := pluginInstalledInMeta(meta, comp)
	if !installed {
		t.Fatal("expected cc-octo to be installed")
	}

	// When installed, upgrade path is handled by computePluginHint (separate logic)
	// InstallVersion should be empty — web should gate install button off
	hasUpdate, latestVer := computePluginHint("claude", meta, latestVersions)
	if !hasUpdate || latestVer != "1.1.0" {
		t.Errorf("expected upgrade hint (true, 1.1.0), got (%v, %q)", hasUpdate, latestVer)
	}
}

// TestInstallVersionHint_OpenclawSymmetric tests openclaw symmetric behavior.
func TestInstallVersionHint_OpenclawSymmetric(t *testing.T) {
	// OpenClaw runtime without octo plugin
	metaNoOcto := `{"cli_version":"0.3.0"}`
	latestVersions := map[string]string{"octo": "0.7.0"}

	comp, ok := expectedPluginComponent("openclaw")
	if !ok || comp != componentPlugin {
		t.Fatalf("expected octo for openclaw, got %q (ok=%v)", comp, ok)
	}

	installed := pluginInstalledInMeta(metaNoOcto, comp)
	if installed {
		t.Fatal("expected octo NOT installed")
	}

	installVer := latestVersions[comp]
	if installVer != "0.7.0" {
		t.Errorf("got install version %q, want 0.7.0", installVer)
	}

	// Now with octo installed — install version should be empty
	metaWithOcto := `{"cli_version":"0.3.0","plugins":[{"name":"octo","version":"0.6.0"}]}`
	installed = pluginInstalledInMeta(metaWithOcto, comp)
	if !installed {
		t.Fatal("expected octo to be installed")
	}

	// Upgrade path handled by computePluginHint
	hasUpdate, latestVer := computePluginHint("openclaw", metaWithOcto, latestVersions)
	if !hasUpdate || latestVer != "0.7.0" {
		t.Errorf("expected upgrade hint (true, 0.7.0), got (%v, %q)", hasUpdate, latestVer)
	}
}

// TestInstallVersionHint_NoLatestAvailable tests that when no latest version
// is configured, PluginInstallVersion remains empty.
func TestInstallVersionHint_NoLatestAvailable(t *testing.T) {
	meta := `{"cli_version":"0.3.0"}`
	latestVersions := map[string]string{} // no versions configured

	comp, ok := expectedPluginComponent("claude")
	if !ok {
		t.Fatal("expected cc-octo for claude")
	}

	installed := pluginInstalledInMeta(meta, comp)
	if installed {
		t.Fatal("expected cc-octo NOT installed")
	}

	installVer := latestVersions[comp]
	if installVer != "" {
		t.Errorf("expected empty install version when not configured, got %q", installVer)
	}
}
