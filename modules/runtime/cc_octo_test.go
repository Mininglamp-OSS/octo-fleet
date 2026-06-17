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
