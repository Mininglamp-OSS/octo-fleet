package runtime

import (
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// 决策三 SSE 反向派发: runtime_event_log 持久化层.
//
// 每次 fleet → daemon SSE push 都先写 event_log (id 是 SSE event id),
// daemon 重连时带 Last-Event-ID, 从 log 拉 id>last 的 event replay.
//
// scope 同 v3.3.x: owner_uid / space_id 列在 query 中显式 gate, 不靠
// runtime_id 关联反推 (event_log 是独立表, 不 join agent_runtime).

const (
	eventTypeUpgrade            = "upgrade"
	eventTypeBotProvision       = "bot_provision"
	eventTypeManagedBotsChanged = "managed_bots_changed"
)

type eventLogEntry struct {
	ID        int64     `db:"id"`
	RuntimeID int64     `db:"runtime_id"`
	SpaceID   string    `db:"space_id"`
	OwnerUID  string    `db:"owner_uid"`
	EventType string    `db:"event_type"`
	Payload   string    `db:"payload"`
	CreatedAt time.Time `db:"created_at"`
}

type eventLogDB struct {
	session *dbr.Session
	ctx     *config.Context
}

func newEventLogDB(ctx *config.Context) *eventLogDB {
	return &eventLogDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// insert 写入一条新 event, 返回自增 id (用作 SSE event id).
// payload 必须是合法 JSON 字符串 — caller 负责 marshal.
func (d *eventLogDB) insert(runtimeID int64, spaceID, ownerUID, eventType, payloadJSON string) (int64, error) {
	result, err := d.session.InsertBySql(
		`INSERT INTO runtime_event_log (runtime_id, space_id, owner_uid, event_type, payload)
		 VALUES (?, ?, ?, ?, ?)`,
		runtimeID, spaceID, ownerUID, eventType, payloadJSON,
	).Exec()
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// querySince 拉某个 runtime 自 lastEventID 之后的所有 event (用于 SSE 重连
// replay). lastEventID=0 表示拉该 runtime 的全部 (TTL 内的).
//
// owner_uid+space_id 显式 gate: caller (SSE handler) 已经做过 ownership
// SQL gate (A7) 拿到这三元组, 这里 redundant 校验防 caller bug 把别人
// 的 event 拉出来.
func (d *eventLogDB) querySince(runtimeID int64, spaceID, ownerUID string, lastEventID int64, limit int) ([]*eventLogEntry, error) {
	var rows []*eventLogEntry
	_, err := d.session.SelectBySql(
		`SELECT id, runtime_id, space_id, owner_uid, event_type, payload, created_at
		 FROM runtime_event_log
		 WHERE runtime_id=? AND space_id=? AND owner_uid=? AND id>?
		 ORDER BY id ASC
		 LIMIT ?`,
		runtimeID, spaceID, ownerUID, lastEventID, limit,
	).Load(&rows)
	return rows, err
}

// pruneOlderThan 删除 created_at < now-threshold 的 event_log row.
// 由 sweeper goroutine 周期性调用 (默认 24h, 跟 daemon dedup state file
// 对称, 见 plan v6 §3.5).
//
// LIMIT 1000 + for loop batch (plan v6 §3.4 explicit): 避免 daemon 集群
// 几天离线 + burst event 累 1M+ row 时一次 DELETE 锁表几十秒, 同时也
// 避免 binlog 单 statement 暴涨. 每 batch 1000 row 大概 10-50ms, 总
// elapsed 跟数据量线性. 返回累计删除的 row 数.
func (d *eventLogDB) pruneOlderThan(threshold time.Duration) (int64, error) {
	cutoff := time.Now().Add(-threshold)
	const batchSize = 1000
	var total int64
	for {
		result, err := d.session.DeleteFrom("runtime_event_log").
			Where("created_at<?", cutoff).
			Limit(batchSize).
			Exec()
		if err != nil {
			return total, err
		}
		n, _ := result.RowsAffected()
		total += n
		if n < batchSize {
			break
		}
	}
	return total, nil
}
