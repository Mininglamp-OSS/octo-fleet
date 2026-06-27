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
