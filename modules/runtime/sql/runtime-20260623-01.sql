-- +migrate Up

-- device: machine as a first-class entity (env-component design §2.1).
--
-- Identity is the daemon-reported persistent device_uuid; hostname is just a
-- mutable display attribute (multica treated hostname as identity → a pile of
-- legacy_daemon_id + case-drift patches). device is a pure physical entity,
-- not tenant-scoped: device_uuid is globally unique, one machine = one row,
-- so machine-level attributes (status / last_seen_at / os / arch) live in a
-- single place instead of fanning out across (daemon, provider, owner) rows
-- and drifting apart like agent_runtime does. device_uuid is the external
-- identity the daemon reports; the surrogate bigint id is what other tables
-- reference (agent_runtime.device_id, device_component.device_id), resolved
-- from device_uuid at register time.
CREATE TABLE IF NOT EXISTS `device` (
    `id`           bigint       NOT NULL AUTO_INCREMENT,
    `device_uuid`  varchar(100) NOT NULL DEFAULT '' COMMENT 'Daemon-reported persistent device identity (external unique key, not hostname)',
    `hostname`     varchar(200) NOT NULL DEFAULT '' COMMENT 'Machine hostname, display only, mutable',
    `os`           varchar(50)  NOT NULL DEFAULT '' COMMENT 'darwin | linux | windows (from device_info)',
    `arch`         varchar(50)  NOT NULL DEFAULT '' COMMENT 'amd64 | arm64 ... (from device_info)',
    `os_version`   varchar(50)  NOT NULL DEFAULT '' COMMENT 'Operating system version (from device_info)',
    `status`       varchar(20)  NOT NULL DEFAULT 'offline' COMMENT 'online | offline',
    `last_seen_at` datetime     DEFAULT NULL COMMENT 'Last heartbeat timestamp',
    `metadata`     text         COMMENT 'Additional device metadata JSON',
    `created_at`   datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`   datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_device_uuid` (`device_uuid`),
    KEY `idx_status` (`status`),
    KEY `idx_last_seen` (`last_seen_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down

DROP TABLE IF EXISTS `device`;
