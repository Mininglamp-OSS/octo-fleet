package runtime

import (
	"strings"
	"testing"
)

// TestUpsertDaemonKeyedByDeviceSpaceOwner: assert upsertDaemon SQL contains device_id, space_id, owner_uid.
func TestUpsertDaemonKeyedByDeviceSpaceOwner(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "upsertDaemon")
	required := []string{"device_id", "space_id", "owner_uid"}
	for _, keyword := range required {
		if !strings.Contains(body, keyword) {
			t.Errorf("upsertDaemon body must contain %q (keyed by device_id+space_id+owner_uid)", keyword)
		}
	}
}

// TestDaemonSQLScopedByOwnerSpace: assert touchDaemon and listDaemonsBySpaceOwner SQL contains space_id and owner_uid.
func TestDaemonSQLScopedByOwnerSpace(t *testing.T) {
	src := mustReadSource(t, "db.go")

	// touchDaemon: must contain daemon_id, space_id, owner_uid
	touchBody := extractFuncBody(t, src, "touchDaemon")
	for _, keyword := range []string{"daemon_id", "space_id", "owner_uid"} {
		if !strings.Contains(touchBody, keyword) {
			t.Errorf("touchDaemon body must contain %q (scoped by daemon_id+space_id+owner_uid)", keyword)
		}
	}

	// listDaemonsBySpaceOwner: must contain space_id, owner_uid
	listBody := extractFuncBody(t, src, "listDaemonsBySpaceOwner")
	for _, keyword := range []string{"space_id", "owner_uid"} {
		if !strings.Contains(listBody, keyword) {
			t.Errorf("listDaemonsBySpaceOwner body must contain %q (scoped by space_id+owner_uid)", keyword)
		}
	}
}

// TestClampHeartbeatIntervalMs: pure function tests for clampHeartbeatIntervalMs.
func TestClampHeartbeatIntervalMs(t *testing.T) {
	tests := []struct {
		name string
		in   int64
		want int64
	}{
		{"zero returns zero (default)", 0, 0},
		{"below min returns zero", 500, 0},
		{"at min boundary", 1000, 1000},
		{"mid range", 5000, 5000},
		{"at max boundary", 300000, 300000},
		{"above max returns zero", 300001, 0},
		{"negative returns zero", -100, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampHeartbeatIntervalMs(tt.in)
			if got != tt.want {
				t.Errorf("clampHeartbeatIntervalMs(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
