-- +migrate Up

CREATE TABLE IF NOT EXISTS `runtime_provider` (
    `id`                  bigint       NOT NULL AUTO_INCREMENT,
    `name`                varchar(50)  NOT NULL DEFAULT '' COMMENT 'provider key: claude/openclaw/codex/hermes，对应 agent_runtime.provider / bot.runtime_kind',
    `display_name`        varchar(80)  NOT NULL DEFAULT '',
    `binary_name`         varchar(80)  NOT NULL DEFAULT '' COMMENT 'daemon LookPath 用的可执行名',
    `upgrade_timeout_sec` int          NOT NULL DEFAULT 600 COMMENT 'fleet sweeper / daemon exec timeout 参考',
    `status`              varchar(16)  NOT NULL DEFAULT 'active' COMMENT 'active | disabled',
    `sort_order`          int          NOT NULL DEFAULT 0,
    `created_at`          datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`          datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_name` (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

INSERT INTO `runtime_provider` (name, display_name, binary_name, upgrade_timeout_sec, status, sort_order) VALUES
  ('claude',   'Claude',   'claude',   600,  'active',   10),
  ('openclaw', 'OpenClaw', 'openclaw', 720,  'active',   20),
  ('codex',    'Codex',    'codex',    600,  'disabled', 30),
  ('hermes',   'Hermes',   'hermes',   1200, 'disabled', 40)
ON DUPLICATE KEY UPDATE display_name=VALUES(display_name), binary_name=VALUES(binary_name),
  upgrade_timeout_sec=VALUES(upgrade_timeout_sec), sort_order=VALUES(sort_order);
-- 注:故意不更新 status —— 重跑/人工 enable/disable 过的 status 不被种子覆盖。

-- +migrate Down

DROP TABLE IF EXISTS `runtime_provider`;
