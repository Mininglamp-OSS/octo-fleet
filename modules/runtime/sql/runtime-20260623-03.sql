-- +migrate Up

-- Link agent_runtime to its owning device (device extraction, see
-- runtime-20260623-01 / -02 and design doc 23-daemon-cli-plugins-fleet).
--
-- agent_runtime retreats to a pure runtime dimension; machine-level facts
-- move to `device`. device_id is a convention FK (references device.id, no
-- enforced constraint — same style as bot.runtime_id) resolved at register
-- time from the daemon-reported fingerprint_id. One device may host many
-- daemon_ids, so multiple agent_runtime rows can share one device_id
-- (many-to-one). 0 = not yet linked (pre-backfill rows); backfill populates
-- it later. Kept out of the unique key so the upsert path does not depend on
-- a device row already existing.
ALTER TABLE `agent_runtime`
  ADD COLUMN `device_id` bigint NOT NULL DEFAULT 0
    COMMENT 'FK device.id (by convention, not enforced); 0=not yet linked'
    AFTER `daemon_id`,
  ADD KEY `idx_device` (`device_id`);

-- +migrate Down

ALTER TABLE `agent_runtime`
  DROP INDEX `idx_device`,
  DROP COLUMN `device_id`;
