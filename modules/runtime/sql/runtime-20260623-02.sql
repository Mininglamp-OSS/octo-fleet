-- +migrate Up

-- device_component: the heart of the model (env-component design §2.2).
-- One row = the state of (one device × one managed component).
--
-- Key invariant: desired_* is written by the control plane, reported_* is
-- written by the daemon; the two column groups never cross. When
-- desired_version == reported_version → in_sync; otherwise → drifted, which
-- triggers convergence.
--
-- Components are an open set (node / cli / various npm -g plugins / proxy ...),
-- described by component_type + name + component_key rather than a separate
-- table per component class. Following this repo's conventions: no real FK
-- (device_id references device.id by comment only) and enum values documented
-- in COMMENT instead of CHECK.
CREATE TABLE IF NOT EXISTS `device_component` (
    `id`               bigint       NOT NULL AUTO_INCREMENT,
    `device_id`        bigint       NOT NULL DEFAULT 0 COMMENT 'FK device.id (by convention, not enforced)',
    `component_type`   varchar(20)  NOT NULL DEFAULT '' COMMENT 'package-manager / runtime type: nodejs (future: go | pip | brew ...)',
    `name`             varchar(120) NOT NULL DEFAULT '' COMMENT 'Logical component name, e.g. octo-daemon',
    `component_key`    varchar(200) NOT NULL DEFAULT '' COMMENT 'Install identifier, e.g. npm package name @mininglamp-oss/octo-daemon',

    -- desired state (control plane writes)
    `desired_version`  varchar(50)  NOT NULL DEFAULT '' COMMENT 'Target version; empty = component not managed',
    `desired_channel`  varchar(20)  NOT NULL DEFAULT 'stable' COMMENT 'stable | beta | pinned',
    `auto_upgrade`     tinyint(1)   NOT NULL DEFAULT 0 COMMENT '1 = converge to desired automatically',

    -- reported state (daemon writes)
    `reported_version` varchar(50)  NOT NULL DEFAULT '' COMMENT 'Actual version installed on the machine',
    `reported_at`      datetime     DEFAULT NULL COMMENT 'Last report timestamp',

    -- derived state
    `state`            varchar(20)  NOT NULL DEFAULT 'unknown' COMMENT 'unknown | in_sync | drifted | upgrading | failed',

    `created_at`       datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`       datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_device_component` (`device_id`, `component_type`, `name`),
    KEY `idx_device_state` (`device_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down

DROP TABLE IF EXISTS `device_component`;
