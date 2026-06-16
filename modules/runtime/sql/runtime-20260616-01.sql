-- +migrate Up

-- PR2-fleet: Server Ping removal — drop the runtime_ping table.
--
-- ⚠️ DEPLOY ORDERING: a still-running OLD fleet binary would hit the dropped
-- table via pingInit/pingReport. Deploy safely by draining/replacing old fleet
-- instances before this migration runs (or a stop-the-world deploy). The new
-- binary's daemon report route is a no-op touching no storage, so once it's
-- live the DROP is safe.
--
-- NOTE (review: Jerry-Xin): we intentionally do NOT purge historical
-- event_type='ping' rows from runtime_event_log here. That table has no index
-- on event_type, so a blanket DELETE would full-scan + delete in a single
-- transaction on a high-volume fleet (long locks / binlog pressure at deploy
-- time, and the 24h TTL sweeper is app-driven so it may lag at migration time).
-- It is also unnecessary: ping events are no longer produced (dispatchPing
-- removed), and any leftover ping row replayed via querySince is inert — an
-- upgraded daemon advances its cursor past the unknown event; an old daemon
-- hits the no-op /v1/daemon/ping/:id (200) and advances too. The 24h event_log
-- TTL sweeper reaps residual rows. If an operator wants them gone immediately,
-- run a batched/indexed DELETE out-of-band, not in this schema migration.
DROP TABLE IF EXISTS `runtime_ping`;

-- +migrate Down

-- Down is provided for rollback symmetry only (framework runs Up). Recreate the
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
