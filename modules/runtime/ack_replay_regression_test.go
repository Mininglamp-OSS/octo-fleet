// ackBot replay-guard regression net.
//
// The pre-fix ackBot called updateBotStatus (`UPDATE bot SET status=?,
// error_msg=? WHERE id=?`) — no current-status guard, and the
// claim_token was never cleared after a successful ack. Combined with
// soft-delete (archiveBot sets status=archived but keeps the row AND the
// claim_token), a delayed or duplicated daemon ack carrying the same
// claim_token could re-pass every check and flip an archived/terminal
// bot back to active/failed — resurrecting a bot the user already
// deleted. The daemon's fault-tolerance *replays* acks (a failed ack is
// not marked done, so SSE replay / heartbeat retries it), so duplicate
// delivery is normal traffic, not a rare anomaly.
//
// Fix invariant (asserted at source level, matching this package's
// harness-free regression style — see owner_regression_test.go for why
// fleet has no DB integration harness):
//
//   1. the ack write is atomic on (status=dispatched AND claim_token=?),
//      so a replayed ack against an archived/terminal bot matches no row;
//   2. the write clears claim_token, so the same token can't be replayed;
//   3. on zero rows, ackBot re-checks current status: an already-applied
//      ack (bot already in the requested status) is idempotent → OK, so
//      the replaying daemon can mark it done; only a genuine conflict
//      (archived / other terminal / token rotated) returns 409. Returning
//      409 for the idempotent case would make the daemon retry forever.

package runtime

import (
	"strings"
	"testing"
)

func TestAckBotStatus_AtomicStatusAndTokenRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "ackBotStatus")

	// Assert the FULL where predicate, not a bare `status=?` — the SET
	// clause also contains `status=?`, so a substring check for it would
	// still pass if someone dropped the WHERE status guard. The atomic
	// guard only holds with all three terms present.
	if !strings.Contains(body, "WHERE id=? AND status=? AND claim_token=?") {
		t.Errorf("ackBotStatus must gate the UPDATE on the full predicate "+
			"`WHERE id=? AND status=? AND claim_token=?` so a replayed/stale ack "+
			"against an archived or terminal bot matches no row.\n\nbody:\n%s", body)
	}
	if !strings.Contains(body, "botStatusDispatched") {
		t.Errorf("ackBotStatus must gate the update on botStatusDispatched specifically.\n\nbody:\n%s", body)
	}
	// Clear the token in SET (anti-replay: a second ack with the same
	// token must no longer match any row).
	if !strings.Contains(body, "claim_token=''") {
		t.Errorf("ackBotStatus must clear claim_token in SET so the same token can't be replayed.\n\nbody:\n%s", body)
	}
}

func TestAckBot_RoutesThroughAtomicGuardRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "ackBot")

	// ackBot must use the atomic guard, not the unconstrained
	// updateBotStatus (WHERE id=? only) that has no replay protection.
	if !strings.Contains(body, "ackBotStatus") {
		t.Errorf("ackBot must use the atomic ackBotStatus guard.\n\nbody:\n%s", body)
	}
	if strings.Contains(body, "updateBotStatus") {
		t.Errorf("ackBot must NOT call updateBotStatus — its `WHERE id=?` has no "+
			"current-status / claim_token guard, so it cannot prevent replay.\n\nbody:\n%s", body)
	}
	// a zero-row result must reserve 409 for a genuine conflict.
	if !strings.Contains(body, "http.StatusConflict") {
		t.Errorf("ackBot must return 409 Conflict when a zero-row ack is a genuine "+
			"state conflict (archived / terminal / token rotated).\n\nbody:\n%s", body)
	}
}

// Idempotent-replay guard: the daemon replays acks on any non-2xx, so a
// duplicate ack whose effect already landed must return OK — not 409 —
// or the daemon never marks it done and retries forever.
func TestAckBot_IdempotentReplayReturnsOKRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "ackBot")

	// On zero rows affected, ackBot must re-query current status to tell an
	// already-applied ack from a real conflict. One query at the top + one
	// re-query in the zero-rows branch = at least two queryBotByID calls.
	if strings.Count(body, "queryBotByID") < 2 {
		t.Errorf("ackBot must re-query bot status in the zero-rows branch to "+
			"distinguish an idempotent replay from a real conflict.\n\nbody:\n%s", body)
	}
	// The idempotent branch returns OK (one more ResponseOK than the single
	// terminal one), so the replaying daemon can mark the command done.
	if strings.Count(body, "ResponseOK") < 2 {
		t.Errorf("ackBot must return OK when a replayed ack finds the bot already "+
			"in the requested status, not only 409.\n\nbody:\n%s", body)
	}
}

// A successful ack clears claim_token, so a replayed ack carries a token
// the row no longer has. The idempotent short-circuit (bot already in the
// requested status) MUST come before the claim_token check — otherwise the
// `m.ClaimToken == ""` guard 409s the replay and the daemon, which retries
// on any non-2xx, loops forever. archived bots never match (status is
// "archived", never active/failed), so resurrection is still blocked.
func TestAckBot_IdempotentBeforeTokenCheckRegressionNet(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "ackBot")

	idemIdx := strings.Index(body, "if m.Status == req.Status")
	if idemIdx < 0 {
		t.Fatalf("ackBot must short-circuit to OK when m.Status == req.Status "+
			"(an already-applied ack), so a token-cleared replay is idempotent.\n\nbody:\n%s", body)
	}
	// Anchor on the actual `if` statements, not bare identifiers — the
	// explanatory comment above the short-circuit also mentions m.ClaimToken.
	tokenIdx := strings.Index(body, "if m.ClaimToken")
	if tokenIdx < 0 || idemIdx > tokenIdx {
		t.Errorf("the idempotent `m.Status == req.Status` short-circuit must come BEFORE "+
			"the claim_token check (a successful ack clears the token, so the replay "+
			"would otherwise be 409'd at the token guard and never reach idempotency).\n\nbody:\n%s", body)
	}
}
