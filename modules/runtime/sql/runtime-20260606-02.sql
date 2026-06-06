-- +migrate Up

-- v3.3.1 §C.1 (Jerry-Xin Critical 1, three-round): runtime_ping owner_uid
-- column. This is the paired schema fix for runtime-20260606-01.sql
-- (agent_runtime 4-tuple unique key that allowed shared (space, daemon,
-- provider) across owners). Without per-row owner_uid on runtime_ping,
-- claimPendingPing / getPing / report can resolve a ping to either of
-- the two owner-rows on a collision — user B's heartbeat could claim
-- user A's pending ping, and vice versa.
--
-- Migration is up-only in practice: rolling back removes per-row owner
-- info which downstream code now relies on. If a rollback is needed,
-- revert the deploy AND keep this schema (the new column is a safe
-- superset; pre-v3.3.1 SQL ignores it).

ALTER TABLE `runtime_ping`
  ADD COLUMN `owner_uid` VARCHAR(64) NOT NULL DEFAULT '' AFTER `daemon_id`,
  ADD INDEX `idx_runtime_ping_owner_space_daemon` (`owner_uid`, `space_id`, `daemon_id`);

-- +migrate Down

ALTER TABLE `runtime_ping`
  DROP INDEX `idx_runtime_ping_owner_space_daemon`,
  DROP COLUMN `owner_uid`;
