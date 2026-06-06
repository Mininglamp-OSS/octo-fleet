-- +migrate Up

-- v3 §4.4 (Jerry-Xin Critical 2, decision point #3 = B):
--
-- agent_runtime's unique key was (space_id, daemon_id, provider), which
-- meant two users sharing a space could clobber each other by reusing the
-- same daemon_id — register's ON DUPLICATE KEY UPDATE overwrote the prior
-- owner's metadata silently. v2 plan considered an A-option pre-check
-- (`SELECT owner_uid ... WHERE space=? AND daemon=? AND provider=?` then
-- 403 on owner mismatch) but a plan-reviewer audit caught the TOCTOU race
-- (two callers SELECT both miss, both INSERT, second ON DUPLICATE
-- overwrites first). Schema migration is the only race-free fix.
--
-- New 4-tuple key co-locates (space, daemon, provider, owner_uid):
--   - same owner re-registering same daemon: ON DUPLICATE refreshes their
--     row (no behavior change for legitimate use).
--   - two owners sharing (space, daemon, provider): two distinct rows
--     coexist; per-row owner_uid scopes all downstream queries
--     (heartbeat, claim*, listActiveBotsForDaemon §4.3).
--
-- Existing rows: the old uk_space_daemon_provider already enforced that
-- only one row per (space, daemon, provider) existed at any time, so no
-- pre-migration data cleanup is needed — the old set is a strict subset
-- of what the new key permits. A pre-deploy `SELECT space_id, daemon_id,
-- provider, COUNT(*) FROM agent_runtime GROUP BY 1,2,3 HAVING COUNT(*)>1`
-- should return zero; if it does not, investigate before applying.

ALTER TABLE `agent_runtime`
  DROP INDEX `uk_space_daemon_provider`;

ALTER TABLE `agent_runtime`
  ADD UNIQUE KEY `uk_space_daemon_provider_owner`
    (`space_id`, `daemon_id`, `provider`, `owner_uid`);

-- +migrate Down

ALTER TABLE `agent_runtime`
  DROP INDEX `uk_space_daemon_provider_owner`;

ALTER TABLE `agent_runtime`
  ADD UNIQUE KEY `uk_space_daemon_provider`
    (`space_id`, `daemon_id`, `provider`);
