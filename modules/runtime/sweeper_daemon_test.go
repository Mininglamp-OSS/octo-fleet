package runtime

import (
	"strings"
	"testing"
)

// TestRunSweeperInvokesDaemonSweep verifies that runSweeper's loop body
// calls markStaleDaemonsOffline (daemon 判死接入主循环) and logs the
// expected message.
func TestRunSweeperInvokesDaemonSweep(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "runSweeper")

	// The function body must contain the daemon sweep call.
	if !strings.Contains(body, "markStaleDaemonsOffline(") {
		t.Errorf("runSweeper body must invoke markStaleDaemonsOffline (daemon 判死接入 sweeper)")
	}

	// And it must log the outcome for observability.
	if !strings.Contains(body, "marked stale daemons offline") {
		t.Errorf("runSweeper body must contain log line 'marked stale daemons offline'")
	}
}

// TestDaemonSweepInsideGraceGuard asserts that the daemon sweep call sits
// inside the graceUntil guard — i.e., it appears after the graceUntil
// reference in the same conditional block. This prevents a future refactor
// from accidentally moving the daemon sweep outside the grace window, which
// would cause fleet restarts to immediately mark all daemons offline before
// they've had a chance to send their first post-restart heartbeat.
func TestDaemonSweepInsideGraceGuard(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "runSweeper")

	idxGrace := strings.Index(body, "graceUntil")
	idxDaemon := strings.Index(body, "markStaleDaemonsOffline(")

	if idxGrace < 0 {
		t.Fatalf("runSweeper body must reference graceUntil (grace period guard)")
	}
	if idxDaemon < 0 {
		t.Fatalf("runSweeper body must invoke markStaleDaemonsOffline")
	}
	if idxDaemon <= idxGrace {
		t.Errorf("markStaleDaemonsOffline must appear AFTER graceUntil check (inside grace guard); got grace at %d, daemon sweep at %d", idxGrace, idxDaemon)
	}
}

// TestDaemonSweepBeforeRuntimeSweepContinue ensures the daemon sweep call
// is placed before the runtime sweep's `continue` on error, so a DB error
// during runtime sweep can't skip the daemon sweep entirely. Uses precise
// substrings to avoid confusion between the two similarly-named functions:
// rt.db.markStaleDaemonsOffline( vs rt.db.markStaleOffline(.
func TestDaemonSweepBeforeRuntimeSweepContinue(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "runSweeper")

	idxDaemon := strings.Index(body, "rt.db.markStaleDaemonsOffline(")
	idxRuntime := strings.Index(body, "rt.db.markStaleOffline(")

	if idxDaemon < 0 {
		t.Fatalf("runSweeper body must invoke rt.db.markStaleDaemonsOffline(")
	}
	if idxRuntime < 0 {
		t.Fatalf("runSweeper body must invoke rt.db.markStaleOffline(")
	}

	// Daemon sweep must come before runtime sweep.
	if idxDaemon >= idxRuntime {
		t.Errorf("daemon sweep (markStaleDaemonsOffline) must appear before runtime sweep (markStaleOffline); got daemon at %d, runtime at %d", idxDaemon, idxRuntime)
	}

	// The runtime sweep's continue (on err) must appear after its own call.
	// Extract the substring starting from runtime sweep to find its continue.
	runtimeSweepPart := body[idxRuntime:]
	idxContinueInRuntime := strings.Index(runtimeSweepPart, "continue")
	if idxContinueInRuntime < 0 {
		t.Fatalf("runtime sweep block must contain continue (error path)")
	}
	// continue should be after the markStaleOffline call within that block.
	if idxContinueInRuntime == 0 {
		t.Errorf("runtime sweep continue should not be at the very start of the runtime sweep block")
	}
}

// TestRunSweeperGCsDaemonsAndOrphanDevices verifies the GC pass for daemon rows
// and orphan devices is wired into runSweeper (zombie-device cleanup), and that
// it runs before the runtime GC's err continue (so a runtime-GC error can't skip
// it). Order: daemon GC before orphan-device GC (a referenced device must keep
// its row).
func TestRunSweeperGCsDaemonsAndOrphanDevices(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "runSweeper")

	idxDaemonGC := strings.Index(body, "deleteStaleDaemons(")
	idxOrphanGC := strings.Index(body, "deleteOrphanDevices(")
	idxRuntimeGC := strings.Index(body, "deleteStaleOffline(")

	if idxDaemonGC < 0 {
		t.Errorf("runSweeper must invoke deleteStaleDaemons (daemon GC)")
	}
	if idxOrphanGC < 0 {
		t.Errorf("runSweeper must invoke deleteOrphanDevices (orphan device GC)")
	}
	if idxRuntimeGC < 0 {
		t.Fatalf("runSweeper must invoke deleteStaleOffline (runtime GC)")
	}
	// daemon GC before orphan device GC
	if idxDaemonGC >= 0 && idxOrphanGC >= 0 && idxDaemonGC >= idxOrphanGC {
		t.Errorf("deleteStaleDaemons must run before deleteOrphanDevices; got daemon at %d, orphan at %d", idxDaemonGC, idxOrphanGC)
	}
	// daemon + orphan GC before the runtime GC's err continue (place before it)
	if idxDaemonGC >= idxRuntimeGC || idxOrphanGC >= idxRuntimeGC {
		t.Errorf("daemon/orphan GC must run before runtime GC (deleteStaleOffline) so its err continue can't skip them")
	}
}

// TestDeleteStaleDaemonsFiltersOfflineOnly: the daemon GC SQL must only delete
// offline rows past the threshold (never an online daemon).
func TestDeleteStaleDaemonsFiltersOfflineOnly(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "deleteStaleDaemons")
	if !strings.Contains(body, "offline") {
		t.Errorf("deleteStaleDaemons must filter status=offline (never GC an online daemon)")
	}
	if !strings.Contains(body, "last_seen_at") {
		t.Errorf("deleteStaleDaemons must filter by last_seen_at threshold")
	}
}

// TestDeleteOrphanDevicesChecksBothReferrers: an orphan device is one referenced
// by NEITHER daemon NOR agent_runtime — the SQL must check both tables, else a
// device still hosting runtimes (or daemons) could be wrongly deleted.
func TestDeleteOrphanDevicesChecksBothReferrers(t *testing.T) {
	src := mustReadSource(t, "db.go")
	body := extractFuncBody(t, src, "deleteOrphanDevices")
	for _, ref := range []string{"daemon", "agent_runtime"} {
		if !strings.Contains(body, ref) {
			t.Errorf("deleteOrphanDevices must exclude devices referenced by %q", ref)
		}
	}
}
