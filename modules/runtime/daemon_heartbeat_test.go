package runtime

import (
	"strings"
	"testing"
)

// TestDaemonHeartbeatRouteRegistered: assert Route registers /daemons/heartbeat in daemon group.
func TestDaemonHeartbeatRouteRegistered(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "Route")
	if !strings.Contains(body, `"/daemons/heartbeat"`) {
		t.Error("Route must register /daemons/heartbeat")
	}
	if !strings.Contains(body, "rt.daemonHeartbeat") {
		t.Error("Route must wire rt.daemonHeartbeat for /daemons/heartbeat")
	}
}

// TestDaemonHeartbeatScoped: assert handler takes uid/space_id from auth, calls touchDaemon + clamp, rows==0 warns but still ResponseEmpty.
func TestDaemonHeartbeatScoped(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "daemonHeartbeat")

	// Auth fields taken from context, not body
	for _, keyword := range []string{`MustGet("uid")`, `MustGet("space_id")`} {
		if !strings.Contains(body, keyword) {
			t.Errorf("daemonHeartbeat must take %s from auth context", keyword)
		}
	}

	// Calls touchDaemon
	if !strings.Contains(body, "touchDaemon") {
		t.Error("daemonHeartbeat must call touchDaemon")
	}

	// Calls clampHeartbeatIntervalMs
	if !strings.Contains(body, "clampHeartbeatIntervalMs") {
		t.Error("daemonHeartbeat must call clampHeartbeatIntervalMs")
	}

	// rows==0 branch warns but still returns success
	if !strings.Contains(body, "rows == 0") {
		t.Error("daemonHeartbeat must check rows == 0")
	}
	if !strings.Contains(body, "ResponseEmpty(c)") {
		t.Error("daemonHeartbeat must call ResponseEmpty even when rows == 0")
	}
}

// TestDaemonDeviceUUIDMismatchScoped: assert helper SQL contains daemon_id+space_id+owner_uid triple.
func TestDaemonDeviceUUIDMismatchScoped(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "daemonDeviceUUIDMismatch")
	for _, keyword := range []string{"daemon_id", "space_id", "owner_uid"} {
		if !strings.Contains(body, keyword) {
			t.Errorf("daemonDeviceUUIDMismatch SQL must contain %q (scoped by daemon_id+space_id+owner_uid)", keyword)
		}
	}
}
