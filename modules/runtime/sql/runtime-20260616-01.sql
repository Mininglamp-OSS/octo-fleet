-- +migrate Up

-- PR2-fleet: Server Ping removal.
--
-- ⚠️ DEPLOY ORDERING (codex review): this migration DROPs runtime_ping in the
-- same release that removes the ping table callers. A still-running OLD fleet
-- binary would hit the dropped table via pingInit/pingReport. Deploy safely by
-- either (a) draining/replacing all old fleet instances before this migration
-- runs, or (b) a stop-the-world deploy. The new binary's report route is a
-- no-op that touches no storage, so once it's live the DROP is safe.
--
-- 1) Purge historical ping events from the SSE event log. Old daemons
--    reconnect with a stale Last-Event-ID and replay id>last rows via
--    querySince; a leftover event_type='ping' row would replay a now-
--    removed event type. Deleting them first prevents replay hazards /
--    cursor stalls during the grayscale window. event_type='ping' is no
--    longer produced (dispatchPing removed in this PR).
--
--    Scope note (codex review): runtime_event_log is bounded by a 24h TTL
--    sweeper (event_log.go pruneOlderThan), so this DELETE only touches at
--    most ~24h of ping rows, not unbounded history — lock/binlog impact is
--    bounded. If a deployment has an unusually large event_log, run this
--    DELETE out-of-band (batched) before applying the migration instead.
DELETE FROM `runtime_event_log` WHERE `event_type` = 'ping';

-- 2) Drop the ping table. The DB layer and all callers are removed in
--    this PR; the daemon report route is now a no-op that touches no
--    storage, so dropping the table is safe.
DROP TABLE IF EXISTS `runtime_ping`;

-- +migrate Down

-- Down is provided for rollback symmetry only (framework runs Up). The
-- DELETE above is not reversible (purged rows are gone); recreate the
-- ping table matching its final shape: runtime-20260425-02 (base) +
-- runtime-20260606-02 (owner_uid column + index).
CREATE TABLE IF NOT EXISTS `runtime_ping` (
    `id`         varchar(64)  NOT NULL DEFAULT '' COMMENT 'Ping ID',
    `space_id`   varchar(40)  NOT NULL DEFAULT '',
    `daemon_id`  varchar(100) NOT NULL DEFAULT '',
    `owner_uid`  varchar(64)  NOT NULL DEFAULT '',
    `server_ts`  bigint       NOT NULL DEFAULT 0 COMMENT 'Server timestamp ms',
    `daemon_ts`  bigint       NOT NULL DEFAULT 0 COMMENT 'Daemon timestamp ms',
    `rtt_ms`     bigint       NOT NULL DEFAULT 0,
    `status`     varchar(20)  NOT NULL DEFAULT 'pending' COMMENT 'pending/done/timeout',
    `created_at` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    KEY `idx_space_daemon_status` (`space_id`, `daemon_id`, `status`, `created_at`),
    KEY `idx_created` (`created_at`),
    KEY `idx_runtime_ping_owner_space_daemon` (`owner_uid`, `space_id`, `daemon_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
