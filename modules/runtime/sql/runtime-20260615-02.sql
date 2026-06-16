-- +migrate Up

-- 去 codex/hermes:把已存在的 online 行置 offline(soft hide,不物理删,
-- 保留历史 bot 关联)。新注册被 register active gate 挡住、列表被 active 过滤。
UPDATE `agent_runtime` SET `status`='offline'
 WHERE `provider` IN ('codex','hermes') AND `status`='online';

-- +migrate Down

-- 无需还原:重新 enable codex/hermes 时由 runtime_provider.status 改回 active
-- + daemon 重新 register 即可恢复 online,不靠这条 migration 回滚。
SELECT 1;
