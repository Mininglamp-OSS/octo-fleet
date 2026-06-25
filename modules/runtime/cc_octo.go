package runtime

import "encoding/json"

// componentCcOcto is claude's octo-adapter plugin component (the cc-channel-octo
// gateway), the analogue of openclaw's bundled componentPlugin ("octo"). The
// daemon reports its version in metadata.plugins as {name:"cc-octo"}, and the
// web keys its version display + upgrade button off the same string — this
// constant must stay in sync across all three repos.
const componentCcOcto = "cc-octo"

// expectedPluginComponent maps a provider to the octo-adapter plugin component
// it carries: openclaw → "octo", claude → "cc-octo". Single source of truth for
// the provider↔plugin relationship in fleet (mirrors the web octoComponentName
// map and the daemon's cc-octo reporting).
func expectedPluginComponent(provider string) (string, bool) {
	switch provider {
	case "openclaw":
		return componentPlugin, true
	case "claude":
		return componentCcOcto, true
	}
	return "", false
}

// isPluginComponent reports whether a component is an octo-adapter plugin (vs.
// the daemon or a provider CLI). Drives upgrade dispatch and sweeper bucketing.
func isPluginComponent(component string) bool {
	return component == componentPlugin || component == componentCcOcto
}

// validPluginForProvider reports whether a plugin component belongs to the given
// provider (octo↔openclaw, cc-octo↔claude). Guards createPluginUpgradeTask so a
// mismatched (component, provider) pair can't create a bogus upgrade order.
func validPluginForProvider(component, provider string) bool {
	exp, ok := expectedPluginComponent(provider)
	return ok && exp == component
}

// pluginInstalledInMeta reports whether a plugin with the given name exists in
// the runtime's metadata.plugins array. Mirrors the parse logic in computePluginHint.
func pluginInstalledInMeta(metadataJSON, name string) bool {
	if metadataJSON == "" {
		return false
	}
	var meta struct {
		Plugins []pluginInfo `json:"plugins"`
	}
	if json.Unmarshal([]byte(metadataJSON), &meta) != nil {
		return false
	}
	for _, p := range meta.Plugins {
		if p.Name == name {
			return true
		}
	}
	return false
}

// computePluginHint decides whether a runtime's octo-adapter plugin has an
// available update. Pure (no DB) so version-hint logic is unit testable: it
// picks the expected plugin name for the provider, finds it in the runtime's
// metadata.plugins, and compares against the configured latest version.
func computePluginHint(provider, metadataJSON string, latest map[string]string) (hasUpdate bool, latestVersion string) {
	name, ok := expectedPluginComponent(provider)
	if !ok || metadataJSON == "" {
		return false, ""
	}
	latestVer := latest[name]
	if latestVer == "" {
		return false, ""
	}
	var meta struct {
		Plugins []pluginInfo `json:"plugins"`
	}
	if json.Unmarshal([]byte(metadataJSON), &meta) != nil {
		return false, ""
	}
	for _, p := range meta.Plugins {
		if p.Name == name && p.Version != "" && isVersionOlder(p.Version, latestVer) {
			return true, latestVer
		}
	}
	return false, ""
}

// computeDaemonHint derives the octo-daemon update hint for a device from its
// machine-level components. "current" is the npm-installed octo-daemon
// reported_version — the single authoritative source shared with the upgrade
// gate (createDaemonUpgradeTask via queryDaemonReportedVersion), so the hint and
// the gate never disagree. Returns ok=false when there's nothing to advertise:
// no latest published, no octo-daemon component reported, or already at/above
// latest.
func computeDaemonHint(components []deviceComponentView, latest string) (daemonVersionHint, bool) {
	if latest == "" {
		return daemonVersionHint{}, false
	}
	current := ""
	for _, comp := range components {
		if comp.Name == componentDaemon {
			current = comp.Version
			break
		}
	}
	if current == "" || !isVersionOlder(current, latest) {
		return daemonVersionHint{}, false
	}
	return daemonVersionHint{HasUpdate: true, LatestVersion: latest, Current: current}, true
}
