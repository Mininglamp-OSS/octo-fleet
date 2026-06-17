-- +migrate Up

-- 决策三 SSE 反向派发: runtime_event_log
--
-- fleet → daemon 4 类反向派发 (ping / upgrade / bot_provision /
-- managed_bots_changed) 从 heartbeat response 夹带改 SSE 长连接主动
-- 推送 (5-7s 延迟 → <500ms). event_log 是持久化层:
--   - SSE handler 写入每条 push 的 event (id 自增 = SSE event id)
--   - daemon 重连时带 Last-Event-ID, server 从 log 拉 id>last 的 event 一次性 replay
--
-- 保留 24h (跟 daemon-cli 端 dedup state file 对称), 重连超 24h 的
-- daemon 视为重 register 走全量 snapshot (heartbeat managed_bots).
--
-- 字段 scope 三件套 (runtime_id / space_id / owner_uid) 跟 v3.3.1
-- runtime_ping / v3.3.x 其它 runtime_* 表一致: SQL 处处 owner_uid
-- gate 防 cross-tenant leak.

CREATE TABLE IF NOT EXISTS `runtime_event_log` (
    `id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT 'SSE event id (全局自增, per-runtime 视角下跳号 by design)',
    `runtime_id` BIGINT       NOT NULL                COMMENT 'agent_runtime.id',
    `space_id`   VARCHAR(40)  NOT NULL DEFAULT ''     COMMENT 'Space scope',
    `owner_uid`  VARCHAR(40)  NOT NULL DEFAULT ''     COMMENT 'Robot UID that owns the runtime',
    `event_type` VARCHAR(32)  NOT NULL DEFAULT ''     COMMENT 'ping / upgrade / bot_provision / managed_bots_changed',
    `payload`    JSON         NOT NULL                COMMENT 'Event payload (no secret — bot_provision payload is only command_id, full secret fetched via separate endpoint)',
    `created_at` TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    PRIMARY KEY (`id`),
    KEY `idx_runtime_event` (`runtime_id`, `id`),
    KEY `idx_created` (`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down

DROP TABLE IF EXISTS `runtime_event_log`;
