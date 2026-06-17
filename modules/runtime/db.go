package runtime

import (
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

type runtimeDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newRuntimeDB(ctx *config.Context) *runtimeDB {
	return &runtimeDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

func (d *runtimeDB) upsert(m *agentRuntimeModel) (int64, error) {
	result, err := d.session.InsertBySql(`
		INSERT INTO agent_runtime (space_id, daemon_id, name, provider, runtime_mode, status, version, device_name, device_info, metadata, owner_uid, heartbeat_interval_ms, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE
			name=VALUES(name), status=VALUES(status), version=VALUES(version),
			device_name=VALUES(device_name), device_info=VALUES(device_info),
			metadata=VALUES(metadata),
			heartbeat_interval_ms=IF(VALUES(heartbeat_interval_ms)>0, VALUES(heartbeat_interval_ms), heartbeat_interval_ms),
			last_seen_at=NOW()`,
		m.SpaceID, m.DaemonID, m.Name, m.Provider, m.RuntimeMode,
		m.Status, m.Version, m.DeviceName, m.DeviceInfo, m.Metadata, m.OwnerUID, m.HeartbeatIntervalMs,
	).Exec()
	if err != nil {
		return 0, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		existing, err := d.queryByUniqueKey(m.SpaceID, m.DaemonID, m.Provider, m.OwnerUID)
		if err != nil {
			return 0, err
		}
		if existing != nil {
			id = existing.Id
		}
	}
	return id, nil
}

// queryByUniqueKey looks up the agent_runtime row by the 4-tuple unique
// key (v3 §4.4: owner_uid added to the key so two owners sharing a
// daemon_id in the same space don't collide).
func (d *runtimeDB) queryByUniqueKey(spaceID, daemonID, provider, ownerUID string) (*agentRuntimeModel, error) {
	var m *agentRuntimeModel
	_, err := d.session.Select("*").From("agent_runtime").
		Where("space_id=? AND daemon_id=? AND provider=? AND owner_uid=?", spaceID, daemonID, provider, ownerUID).
		Load(&m)
	return m, err
}

func (d *runtimeDB) queryByID(id int64) (*agentRuntimeModel, error) {
	var m *agentRuntimeModel
	_, err := d.session.Select("*").From("agent_runtime").
		Where("id=?", id).
		Load(&m)
	return m, err
}

func (d *runtimeDB) listBySpaceIDAndOwner(spaceID, ownerUID string, activeProviders []string) ([]*agentRuntimeModel, error) {
	// active 为空 = 没有任何 active provider 可见,返回空(而非退化成不过滤列出全部)。
	if len(activeProviders) == 0 {
		return []*agentRuntimeModel{}, nil
	}
	var list []*agentRuntimeModel
	q := d.session.Select("*").From("agent_runtime").
		Where("space_id=? AND owner_uid=?", spaceID, ownerUID).
		Where("provider IN ?", activeProviders) // dbr 展开为 IN (...)
	_, err := q.OrderDir("status", false).OrderAsc("name").Load(&list)
	return list, err
}

// firstRuntimeIDForDaemon 解析 (space, daemon, owner) 对应的任一**在线**
// runtime id, 用作 daemon-level SSE event (daemon 自身 upgrade) 的
// push 目标. SSE channel 是 per-runtime (决策三 v6 §Q2), 而 daemon-
// upgrade 是 daemon 级事件 — 推到任一在线 runtime 即触发 daemon 处理
// (daemon-side dedup 按 event_type+source_pk 防同事件多 runtime 重复处理).
//
// status='online' 过滤 (F3 caster review final): 若最老 runtime offline
// (CLI 卸载/故障) 推到死 channel 会静默丢, daemon 退 heartbeat 5-7s 兜底
// 失去 SSE 加速意义. 过滤后取最老的在线 runtime, 全 offline 才返 0.
//
// 返回 0 表示该 daemon 没有 online runtime, 这种情况 dispatcher 跳过
// in-mem push, event 仍写 log, daemon 下次连上后走 Last-Event-ID 重放.
func (d *runtimeDB) firstRuntimeIDForDaemon(spaceID, daemonID, ownerUID string) (int64, error) {
	var id int64
	_, err := d.session.SelectBySql(
		`SELECT id FROM agent_runtime
		 WHERE space_id=? AND daemon_id=? AND owner_uid=? AND status='online'
		 ORDER BY id ASC LIMIT 1`,
		spaceID, daemonID, ownerUID,
	).Load(&id)
	return id, err
}

func (d *runtimeDB) updateHeartbeat(id int64) error {
	_, err := d.session.Update("agent_runtime").
		Set("status", "online").
		Set("last_seen_at", dbr.Expr("NOW()")).
		Where("id=?", id).
		Exec()
	return err
}

func (d *runtimeDB) setOffline(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.session.Update("agent_runtime").
		Set("status", "offline").
		Where("id IN ?", ids).
		Exec()
	return err
}

// markStaleOffline marks every "online" agent_runtime whose last_seen_at
// is older than its per-row stale threshold. Per-runtime threshold =
// 3 × heartbeat_interval_ms (or 3 × defaultIntervalMs when the column
// is 0/unset). Returns the number of rows flipped.
func (d *runtimeDB) markStaleOffline(defaultIntervalMs int64) (int64, error) {
	result, err := d.session.UpdateBySql(
		`UPDATE agent_runtime
		    SET status='offline'
		  WHERE status='online'
		    AND last_seen_at < DATE_SUB(NOW(), INTERVAL (IF(heartbeat_interval_ms>0, heartbeat_interval_ms, ?) * 3 / 1000) SECOND)`,
		defaultIntervalMs,
	).Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *runtimeDB) deleteStaleOffline(threshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-threshold)
	result, err := d.session.DeleteFrom("agent_runtime").
		Where("status=? AND last_seen_at < ?", "offline", cutoff).
		Exec()
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (d *runtimeDB) deleteByID(id int64) error {
	_, err := d.session.DeleteFrom("agent_runtime").
		Where("id=?", id).
		Exec()
	return err
}

func (d *runtimeDB) deleteBySpaceAndDaemon(spaceID, daemonID string) error {
	_, err := d.session.DeleteFrom("agent_runtime").
		Where("space_id=? AND daemon_id=?", spaceID, daemonID).
		Exec()
	return err
}

func (d *runtimeDB) queryLatestVersions() (map[string]string, error) {
	var rows []struct {
		Component     string `db:"component"`
		LatestVersion string `db:"latest_version"`
	}
	_, err := d.session.Select("component", "latest_version").From("runtime_latest_version").Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(rows))
	for _, r := range rows {
		result[r.Component] = r.LatestVersion
	}
	return result, nil
}

// upsertLatestVersion inserts or updates a component's latest version + release_meta.
// Source is now the internal admin endpoint (POST /v1/internal/runtime-latest-versions);
// the COS version syncer was removed, so this table is maintained manually.
// 空 releaseMeta 表示"不更新 release_meta"——保留已有值(避免省略该字段时清空
// daemon 自升级所需的 assets/checksums)。新行 release_meta 默认 ''。
func (d *runtimeDB) upsertLatestVersion(component, latestVersion, releaseMeta string) error {
	_, err := d.session.InsertBySql(
		`INSERT INTO runtime_latest_version (component, latest_version, release_meta)
		 VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE latest_version=VALUES(latest_version),
		   release_meta=IF(VALUES(release_meta)='', release_meta, VALUES(release_meta))`,
		component, latestVersion, releaseMeta,
	).Exec()
	return err
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d-%02d-%02d %02d:%02d:%02d",
		t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second())
}

// queryBotInfoByUIDs 查询合法 bot（robot.status=1 + user.robot=1 + 属于当前 space）的显示信息。
// 入参 uids 已 dedupe；只返回合法 bot，不合法 / 跨 space 的 uid 不在结果里。
// 这是为 /v1/runtimes 的 route enrich 服务的，防止 daemon 上报任意 uid 被 enrich 出 user 真名。
// 表名 `user` 反引号包起来，避免 MySQL 对保留字解析差异。
func (d *runtimeDB) queryBotInfoByUIDs(spaceID string, uids []string) (map[string]botInfo, error) {
	if len(uids) == 0 || spaceID == "" {
		return map[string]botInfo{}, nil
	}
	// FLEET MIGRATION: user/robot/space_member tables live in
	// octo-server's schema. Fleet cannot enrich names locally; the
	// browser supplements the display name via a separate server
	// call when needed. Return empty map so callers fall back to
	// raw UIDs.
	return map[string]botInfo{}, nil
}

// queryBotInfoByUIDsLegacy is kept here as reference for the original
// implementation; not invoked from fleet.
func (d *runtimeDB) queryBotInfoByUIDsLegacy(spaceID string, uids []string) (map[string]botInfo, error) {
	if len(uids) == 0 || spaceID == "" {
		return map[string]botInfo{}, nil
	}
	// dbr 的 IN 参数需要 []interface{}
	args := make([]interface{}, 0, len(uids)+1)
	args = append(args, spaceID)
	placeholders := make([]string, len(uids))
	for i, uid := range uids {
		placeholders[i] = "?"
		args = append(args, uid)
	}
	sql := fmt.Sprintf(
		"SELECT u.uid, u.name FROM `user` u "+
			"INNER JOIN robot r ON r.robot_id = u.uid AND r.status = 1 "+
			"INNER JOIN space_member sm ON sm.uid = u.uid AND sm.space_id = ? AND sm.status = 1 "+
			"WHERE u.robot = 1 AND u.uid IN (%s)",
		strings.Join(placeholders, ","),
	)
	var rows []struct {
		UID  string `db:"uid"`
		Name string `db:"name"`
	}
	_, err := d.session.SelectBySql(sql, args...).Load(&rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string]botInfo, len(rows))
	for _, r := range rows {
		result[r.UID] = botInfo{UID: r.UID, Name: r.Name}
	}
	return result, nil
}

// listActiveBotsForDaemon returns bot_uid + workspace_id for every active
// bot whose runtime is hosted by this daemon, scoped to (space, owner).
//
// v3 §4.3 (Jerry-Xin Critical 1): the prior (daemonID-only) signature
// joined bots across owners — same space + same daemon_id (allowed by
// register's non-unique (space,daemon_id,provider) key pre-§4.4) could
// surface another owner's active bot inventory (bot_uid + workspace_id
// leak). Scoping by (space, owner) closes that without changing the
// heartbeat happy-path. Caller (heartbeat) already has both values from
// the agent_runtime row it just loaded — no N+1.
func (d *runtimeDB) listActiveBotsForDaemon(daemonID, spaceID, ownerUID string) ([]struct {
	BotUID      string `json:"bot_uid" db:"bot_uid"`
	WorkspaceID string `json:"workspace_id" db:"workspace_id"`
}, error) {
	type row struct {
		BotUID      string `json:"bot_uid" db:"bot_uid"`
		WorkspaceID string `json:"workspace_id" db:"workspace_id"`
	}
	var rows []row
	_, err := d.session.SelectBySql(
		`SELECT b.bot_uid, b.workspace_id
		   FROM bot b
		  WHERE b.daemon_id=? AND b.space_id=? AND b.owner_uid=?
		    AND b.status='active' AND b.bot_uid!=''`,
		daemonID, spaceID, ownerUID,
	).Load(&rows)
	if err != nil {
		return nil, err
	}
	out := make([]struct {
		BotUID      string `json:"bot_uid" db:"bot_uid"`
		WorkspaceID string `json:"workspace_id" db:"workspace_id"`
	}, len(rows))
	for i, r := range rows {
		out[i].BotUID = r.BotUID
		out[i].WorkspaceID = r.WorkspaceID
	}
	return out, nil
}

// loadProviders 读取 runtime_provider 全表，供 providerRegistry 刷新快照。
func (d *runtimeDB) loadProviders() ([]providerDef, error) {
	var rows []providerDef
	_, err := d.session.SelectBySql(
		"SELECT name, display_name, binary_name, upgrade_timeout_sec, status FROM runtime_provider ORDER BY sort_order ASC, name ASC",
	).Load(&rows)
	return rows, err
}
