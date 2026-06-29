package runtime

import "testing"

// Test_Issue69_DaemonOnlyDeviceVisibleWithoutDeviceID pins the invariant the
// three-tier model promises but the implementation broke (octo-fleet#69):
//
//	a daemon that is registered/online MUST appear in the Devices view —
//	even when device_id == 0 (device_info telemetry was missing or its
//	upsert failed) and even when the machine has zero agent_runtime rows.
//
// device row writes are additive telemetry; their failure must not brick the
// daemon's "empty device + green dot" visibility, which the daemon table is
// the authoritative source for. Before the fix, buildDeviceViews dropped every
// daemon row with device_id <= 0, so a daemon-only machine whose device link
// never resolved was invisible. This test exercises the read path that renders
// the Devices map.
func Test_Issue69_DaemonOnlyDeviceVisibleWithoutDeviceID(t *testing.T) {
	daemons := []*daemonModel{
		// daemon-only machine, device link unresolved (device_id == 0), online.
		{DaemonID: "daemon-no-device", DeviceID: 0, Status: "online",
			LastSeenAt: mustParseTime("2026-06-29T10:00:00Z")},
		// a second daemon-only machine under the same owner must NOT collapse
		// into the first — they are distinct devices.
		{DaemonID: "daemon-no-device-2", DeviceID: 0, Status: "online",
			LastSeenAt: mustParseTime("2026-06-29T10:01:00Z")},
	}
	daemons[0].Id = 11
	daemons[1].Id = 12

	// No device rows resolved (device_info never produced a device_id), and no
	// agent_runtime rows at all (no agent adapter installed).
	result := buildDeviceViews(daemons, map[int64]deviceView{})

	if len(result) != 2 {
		t.Fatalf("daemon-only machines invisible: want 2 device views, got %d — "+
			"daemon-online ⟹ device-visible invariant violated for device_id==0", len(result))
	}

	seen := map[string]deviceView{}
	for _, dv := range result {
		seen[dv.DaemonID] = dv
	}
	for _, id := range []string{"daemon-no-device", "daemon-no-device-2"} {
		dv, ok := seen[id]
		if !ok {
			t.Fatalf("daemon %q missing from Devices view", id)
		}
		if dv.Status != "online" {
			t.Errorf("daemon %q: status = %q, want online (green dot)", id, dv.Status)
		}
		if dv.DaemonID != id {
			t.Errorf("daemon-only row must carry daemon_id as fallback identity, got %q", dv.DaemonID)
		}
	}
}
