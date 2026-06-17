-- +migrate Up

-- per-runtime heartbeat interval: daemon reports its actual interval at
-- register time so fleet's sweeper can compute a per-runtime stale
-- threshold (≈ 3× the reported interval) instead of a hardcoded const.
-- NULL / 0 = unset, sweeper falls back to defaultHeartbeatIntervalMs.
ALTER TABLE `agent_runtime`
  ADD COLUMN `heartbeat_interval_ms` int NOT NULL DEFAULT 0
  COMMENT 'daemon-reported heartbeat interval (ms); 0=use sweeper default';
