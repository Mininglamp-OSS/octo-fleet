package runtime

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
)

func mustParseTime(s string) db.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return db.Time(t)
}

// TestListUsesDaemonAuthoritativeSource verifies that the list handler
// uses listDaemonsBySpaceOwner as the authoritative device source.
func TestListUsesDaemonAuthoritativeSource(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatal(err)
	}
	content := string(src)

	if !strings.Contains(content, "listDaemonsBySpaceOwner") {
		t.Error("list handler should call listDaemonsBySpaceOwner for authoritative device source")
	}
	if !strings.Contains(content, "buildDeviceViews") {
		t.Error("list handler should use buildDeviceViews to merge daemon + device data")
	}
}

// TestQueryDevicesNoStatusColumn ensures queryDevicesWithComponents no longer
// selects the removed device.status column (runtime SQL error guard).
func TestQueryDevicesNoStatusColumn(t *testing.T) {
	src, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatal(err)
	}
	content := string(src)

	// Find the queryDevicesWithComponents function body
	idx := strings.Index(content, "func (d *runtimeDB) queryDevicesWithComponents")
	if idx == -1 {
		t.Fatal("queryDevicesWithComponents not found in db.go")
	}
	funcBody := content[idx:]
	endIdx := strings.Index(funcBody, "\n}\n")
	if endIdx == -1 {
		t.Fatal("cannot find end of queryDevicesWithComponents")
	}
	funcBody = funcBody[:endIdx]

	if strings.Contains(funcBody, `"status"`) {
		t.Error("queryDevicesWithComponents must NOT select device.status column (column removed in fleet-1)")
	}
}

// TestUpsertDeviceNoStatusColumn ensures upsertDevice no longer writes the removed
// device.status and device.last_seen_at columns (runtime SQL error guard, fleet-6).
func TestUpsertDeviceNoStatusColumn(t *testing.T) {
	src, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatal(err)
	}
	content := string(src)

	// Find the upsertDevice function body
	idx := strings.Index(content, "func (d *runtimeDB) upsertDevice")
	if idx == -1 {
		t.Fatal("upsertDevice not found in db.go")
	}
	funcBody := content[idx:]
	endIdx := strings.Index(funcBody, "\n}\n")
	if endIdx == -1 {
		t.Fatal("cannot find end of upsertDevice")
	}
	funcBody = funcBody[:endIdx]

	if strings.Contains(funcBody, "`status`") || strings.Contains(funcBody, "'status'") {
		t.Error("upsertDevice must NOT reference device.status column (column removed in fleet-1)")
	}
	if strings.Contains(funcBody, "last_seen_at") {
		t.Error("upsertDevice must NOT reference device.last_seen_at column (column removed in fleet-1)")
	}
}

// TestBuildDeviceViewsEmptyDeviceVisible verifies that devices with a daemon
// but no runtime row are still visible (empty-device-visible invariant).
func TestBuildDeviceViewsEmptyDeviceVisible(t *testing.T) {
	daemons := []*daemonModel{
		{DeviceID: 5, DaemonID: "d5", Status: "online", LastSeenAt: mustParseTime("2026-06-27T10:00:00Z")},
		{DeviceID: 7, DaemonID: "d7", Status: "offline", LastSeenAt: mustParseTime("2026-06-27T09:00:00Z")},
		{DeviceID: 0, DaemonID: "d0", Status: "online", LastSeenAt: mustParseTime("2026-06-27T10:00:00Z")}, // daemon-only, device link unresolved
	}
	daemons[2].Id = 3 // daemon PK drives the synthetic key for device_id==0 rows

	deviceRows := map[int64]deviceView{
		5: {DeviceID: 5, Name: "host5", Components: []deviceComponentView{{Name: "octo-daemon", Version: "0.0.3"}}},
		// note: device 7 intentionally missing from deviceRows to test fallback
	}

	result := buildDeviceViews(daemons, deviceRows)

	// Device 5 should have full info
	if _, ok := result[5]; !ok {
		t.Fatal("device 5 missing from result")
	}
	if result[5].Status != "online" {
		t.Errorf("device 5 status: want online, got %q", result[5].Status)
	}
	if result[5].DaemonID != "d5" {
		t.Errorf("device 5 daemon_id: want d5, got %q", result[5].DaemonID)
	}

	// Device 7 (empty device, no device row) should still be visible with daemon info
	if _, ok := result[7]; !ok {
		t.Fatal("device 7 (empty device) missing from result — violates empty-device-visible invariant")
	}
	if result[7].Status != "offline" {
		t.Errorf("device 7 status: want offline, got %q", result[7].Status)
	}
	if result[7].DaemonID != "d7" {
		t.Errorf("device 7 daemon_id: want d7, got %q", result[7].DaemonID)
	}

	// Device 0 (daemon-only, device link unresolved) must still be visible —
	// keyed by -daemon.id so it neither collapses nor collides with a real
	// device.id, carrying daemon_id as its fallback identity (octo-fleet#69).
	dv0, ok := result[-3]
	if !ok {
		t.Fatal("daemon-only row (device_id==0) missing from result — violates daemon-online ⟹ device-visible invariant")
	}
	if dv0.DaemonID != "d0" {
		t.Errorf("daemon-only row daemon_id: want d0, got %q", dv0.DaemonID)
	}
	if dv0.Status != "online" {
		t.Errorf("daemon-only row status: want online, got %q", dv0.Status)
	}
}
