-- +migrate Up

-- Task fleet-1: daemon 表新建 + device/agent_runtime 瘦身 (三层数据模型重构第一步)
-- 开发阶段无生产数据,采用全清重建策略。

-- 1. 全清重建: 清空相关 runtime 域表
DELETE FROM `agent_runtime`;
DELETE FROM `device_component`;
DELETE FROM `device`;
DROP TABLE IF EXISTS `daemon`;
DELETE FROM `bot_task`;
DELETE FROM `bot`;
DELETE FROM `runtime_upgrade_task`;
DELETE FROM `runtime_event_log`;
-- runtime_ping 早期迁移已 DROP;用 IF EXISTS 幂等清理,确保无论之前是否被回滚重建都不残留
DROP TABLE IF EXISTS `runtime_ping`;

-- 2. device 表瘦身: 移除 status / last_seen_at (在线性迁到 daemon 层)
ALTER TABLE `device`
  DROP INDEX `idx_status`,
  DROP INDEX `idx_last_seen`,
  DROP COLUMN `status`,
  DROP COLUMN `last_seen_at`;

-- 3. 新建 daemon 表 (接入层 —— 绿点 + 归属 + 鉴权权威源)
CREATE TABLE IF NOT EXISTS `daemon` (
    `id`                    bigint       NOT NULL AUTO_INCREMENT,
    `daemon_id`             varchar(100) NOT NULL DEFAULT '' COMMENT 'Daemon-reported per-(device,space) stable id',
    `device_id`             bigint       NOT NULL DEFAULT 0 COMMENT 'FK device.id (by convention, not enforced)',
    `space_id`              varchar(40)  NOT NULL DEFAULT '' COMMENT 'Space ID (auth scope)',
    `owner_uid`             varchar(40)  NOT NULL DEFAULT '' COMMENT 'Owner UID that registered this daemon (auth scope)',
    `status`                varchar(20)  NOT NULL DEFAULT 'offline' COMMENT 'online | offline — the device green dot',
    `last_seen_at`          datetime     DEFAULT NULL COMMENT 'Last daemon-level heartbeat timestamp',
    `heartbeat_interval_ms` bigint       NOT NULL DEFAULT 0 COMMENT 'Daemon-reported heartbeat interval; 0 = sweeper default',
    `created_at`            datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`            datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_space_owner_daemon` (`space_id`, `owner_uid`, `daemon_id`),
    UNIQUE KEY `uk_device_space_owner` (`device_id`, `space_id`, `owner_uid`),
    KEY `idx_space_owner` (`space_id`, `owner_uid`),
    KEY `idx_device` (`device_id`),
    KEY `idx_last_seen` (`last_seen_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- 4. agent_runtime 瘦身: 移除 device_name / device_info (机器信息查 device 表)
ALTER TABLE `agent_runtime`
  DROP COLUMN `device_name`,
  DROP COLUMN `device_info`;

-- +migrate Down

-- Down 段: 逆回 Up 段的改动

-- 4. agent_runtime 加回 device_name / device_info
ALTER TABLE `agent_runtime`
  ADD COLUMN `device_name` varchar(200) NOT NULL DEFAULT '' COMMENT 'Machine hostname',
  ADD COLUMN `device_info` text COMMENT 'Device metadata JSON';

-- 3. 删除 daemon 表
DROP TABLE IF EXISTS `daemon`;

-- 2. device 加回 status / last_seen_at + 索引
ALTER TABLE `device`
  ADD COLUMN `status` varchar(20) NOT NULL DEFAULT 'offline' COMMENT 'online | offline' AFTER `os_version`,
  ADD COLUMN `last_seen_at` datetime DEFAULT NULL COMMENT 'Last heartbeat timestamp' AFTER `status`,
  ADD KEY `idx_status` (`status`),
  ADD KEY `idx_last_seen` (`last_seen_at`);

-- 1. Down 段不需要恢复 DELETE 的数据 (开发阶段无数据)
