package runtime

import (
	"fmt"
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
		INSERT INTO agent_runtime (space_id, daemon_id, device_id, name, provider, runtime_mode, status, version, device_name, device_info, metadata, owner_uid, heartbeat_interval_ms, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, UTC_TIMESTAMP())
		ON DUPLICATE KEY UPDATE
			device_id=IF(VALUES(device_id)>0, VALUES(device_id), device_id),
			name=VALUES(name), status=VALUES(status), version=VALUES(version),
			device_name=VALUES(device_name), device_info=VALUES(device_info),
			metadata=VALUES(metadata),
			heartbeat_interval_ms=IF(VALUES(heartbeat_interval_ms)>0, VALUES(heartbeat_interval_ms), heartbeat_interval_ms),
			last_seen_at=UTC_TIMESTAMP()`,
		m.SpaceID, m.DaemonID, m.DeviceID, m.Name, m.Provider, m.RuntimeMode,
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

// upsertDevice inserts or refreshes the device row keyed by device_uuid
// (the daemon-reported device identity, parsed from device_info) and returns
// device.id, which callers store as agent_runtime.device_id /
// device_component.device_id. Mirrors upsert's LastInsertId==0 fallback for
// pure-UPDATE hits.
func (d *runtimeDB) upsertDevice(deviceUUID, hostname, os, arch, osVersion string) (int64, error) {
	result, err := d.session.InsertBySql(
		`INSERT INTO device (device_uuid, hostname, os, arch, os_version, status, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, 'online', UTC_TIMESTAMP())
		 ON DUPLICATE KEY UPDATE hostname=VALUES(hostname), os=VALUES(os), arch=VALUES(arch),
		   os_version=VALUES(os_version), status='online', last_seen_at=UTC_TIMESTAMP()`,
		deviceUUID, hostname, os, arch, osVersion,
	).Exec()
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if id == 0 {
		_, err = d.session.SelectBySql("SELECT id FROM device WHERE device_uuid=?", deviceUUID).Load(&id)
		if err != nil {
			return 0, err
		}
	}
	return id, nil
}

// upsertDeviceComponent records the reported state of one (device × component)
// keyed by (device_id, component_type, name). It writes ONLY the reported_*
// columns — desired_* and state are owned by the control plane and must never
// be touched here (env-component §2.2 invariant).
func (d *runtimeDB) upsertDeviceComponent(deviceID int64, ctype, name, componentKey, reportedVersion string) error {
	_, err := d.session.InsertBySql(
		`INSERT INTO device_component (device_id, component_type, name, component_key, reported_version, reported_at)
		 VALUES (?, ?, ?, ?, ?, UTC_TIMESTAMP())
		 ON DUPLICATE KEY UPDATE component_key=VALUES(component_key),
		   reported_version=VALUES(reported_version), reported_at=UTC_TIMESTAMP()`,
		deviceID, ctype, name, componentKey, reportedVersion,
	).Exec()
	return err
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
		Set("last_seen_at", dbr.Expr("UTC_TIMESTAMP()")).
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
		    AND last_seen_at < DATE_SUB(UTC_TIMESTAMP(), INTERVAL (IF(heartbeat_interval_ms>0, heartbeat_interval_ms, ?) * 3 / 1000) SECOND)`,
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

// queryDevicesWithComponents loads the device rows + their reported component
// versions for the given device ids, keyed by device.id (PK). Two queries joined
// in Go; components carry only reported_version (the desired_* / state columns
// are control-plane state, not surfaced here).
func (d *runtimeDB) queryDevicesWithComponents(deviceIDs []int64) (map[int64]deviceView, error) {
	result := make(map[int64]deviceView)
	if len(deviceIDs) == 0 {
		return result, nil
	}

	var devices []struct {
		ID         int64  `db:"id"`
		DeviceUUID string `db:"device_uuid"`
		Hostname   string `db:"hostname"`
		OS         string `db:"os"`
		Arch       string `db:"arch"`
		OSVersion  string `db:"os_version"`
		Status     string `db:"status"`
	}
	_, err := d.session.Select("id", "device_uuid", "hostname", "os", "arch", "os_version", "status").
		From("device").Where("id IN ?", deviceIDs).Load(&devices)
	if err != nil {
		return nil, err
	}

	for _, dev := range devices {
		result[dev.ID] = deviceView{
			DeviceID:   dev.ID,
			DeviceUUID: dev.DeviceUUID,
			Name:       dev.Hostname,
			OS:         dev.OS,
			Arch:       dev.Arch,
			OSVersion:  dev.OSVersion,
			Status:     dev.Status,
			Components: []deviceComponentView{},
		}
	}

	var components []struct {
		DeviceID        int64  `db:"device_id"`
		Name            string `db:"name"`
		ReportedVersion string `db:"reported_version"`
	}
	_, err = d.session.Select("device_id", "name", "reported_version").
		From("device_component").Where("device_id IN ?", deviceIDs).Load(&components)
	if err != nil {
		return nil, err
	}
	for _, comp := range components {
		dv, ok := result[comp.DeviceID]
		if !ok {
			continue
		}
		dv.Components = append(dv.Components, deviceComponentView{
			Name:    comp.Name,
			Version: comp.ReportedVersion,
		})
		result[comp.DeviceID] = dv
	}

	return result, nil
}

// queryDaemonReportedVersion returns the npm-installed octo-daemon version for
// a device, read from device_component.reported_version (name="octo-daemon").
// This is the single authoritative source for the daemon's current version —
// the same one GET /runtimes hints off — so the upgrade gate agrees with the
// hint. Returns "" when the device has no octo-daemon component row (e.g. a
// pre-device daemon, deviceID==0), which the caller treats as unknown.
func (d *runtimeDB) queryDaemonReportedVersion(deviceID int64) (string, error) {
	if deviceID <= 0 {
		return "", nil
	}
	var version string
	_, err := d.session.SelectBySql(
		"SELECT reported_version FROM device_component WHERE device_id=? AND name=?",
		deviceID, componentDaemon,
	).Load(&version)
	return version, err
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
// 空 releaseMeta 表示"不更新 release_meta"——保留已有值。新行 release_meta 默认 ''。
// (release_meta 仅作记录留存;daemon 升级已改为 npm install,不再消费 assets/checksums。)
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
func (d *runtimeDB) listActiveBotsForDaemon(daemonID, spaceID, ownerUID string) ([]managedBot, error) {
	var rows []managedBot
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
	return rows, nil
}

// loadProviders 读取 runtime_provider 全表，供 providerRegistry 刷新快照。
func (d *runtimeDB) loadProviders() ([]providerDef, error) {
	var rows []providerDef
	_, err := d.session.SelectBySql(
		"SELECT name, display_name, binary_name, upgrade_timeout_sec, status FROM runtime_provider ORDER BY sort_order ASC, name ASC",
	).Load(&rows)
	return rows, err
}
