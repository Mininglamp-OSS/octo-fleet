package runtime

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope" // swag @Failure type resolution
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 决策三 SSE 反向派发: fleet 端 long-lived 推送.
//
// 替代 heartbeat response 夹带 pending_upgrade/pending_command
// 的拉模式, 把反向派发改成 SSE 主动推 (延迟 5-7s → <500ms).
//
// 架构 (v6 plan §3.4):
//   - GET /v1/runtimes/{runtime_id}/events
//   - api_key Bearer 走 daemon authMW (复用决策二)
//   - A7 ownership SQL gate: SELECT 1 FROM agent_runtime WHERE id=? AND owner_uid=? AND space_id=?
//     防 daemon A 持自己 api_key 传 runtime_id=daemon-B's → cross-daemon event leak (D1 lesson)
//   - SSE headers: X-Accel-Buffering:no / Cache-Control:no-cache / Connection:keep-alive
//   - Last-Event-ID 重连: replay missed events from runtime_event_log
//   - sseHub: sync.Map[runtime_id → chan eventEnvelope (buffered 16)]
//   - 30s keepalive 防 nginx idle timeout (deploy 配 proxy_read_timeout 1h)
//   - 60s TTL re-verify: align verifyCache TTL (revocation 窗口 §3.7)
//
// dispatcher 入口 (供 createUpgrade / createBotProvision /
// managed_bots CRUD 调用): 先 INSERT event_log (拿自增 id), 再 non-blocking
// publish 到 channel. channel full → 跳过 in-mem 推 (event 已落 log,
// daemon 重连走 Last-Event-ID replay).

const (
	sseKeepaliveInterval = 30 * time.Second

	// sseConnTTL: 跟 verifyCache TTL (60s) 对齐 — server 主动 close, daemon
	// 重连触发 re-verify. revocation 窗口跟决策一+二 trade-off 一致
	// (v3.3.6 §P1 / v6 plan §3.7).
	sseConnTTL = 60 * time.Second

	// sseWriteDeadline: per-write socket deadline (P1, yujiawei R4 review).
	//
	// SSE inline write 在 daemon stalled-but-open (网络抖 / TCP 接收窗满 /
	// 进程 paused — 普通 production 场景) 时会 block 在 kernel socket Write,
	// ctx.Done() / <-ttl.C 这种 select case 救不了正在 block 的 socket write.
	// 没 deadline → goroutine + fd + sseHub channel registration leak 到 OS
	// TCP send timeout (几分钟到几十分钟), sseConnTTL (60s revocation 窗口)
	// 被拉到不可预测, 削弱本 PR 自己声明的 security guarantee.
	//
	// 用 http.NewResponseController(w).SetWriteDeadline(now+sseWriteDeadline)
	// 在每次 write/flush 前 set 一次 — block 达 sseWriteDeadline 后 Write 返
	// err, handler 退出, goroutine/fd/channel 注册及时释放. revocation 窗口
	// 在最坏情况只从 60s 拉到 60s+10s, 保住 sseConnTTL guarantee.
	//
	// 数值关系: sseWriteDeadline (10s) < sseKeepaliveInterval (30s) <
	// sseConnTTL (60s). 选 10s 因为: 远大于正常 TCP write (毫秒级 + nginx
	// <1s) 不误杀健康抖动; 远小于 sseConnTTL 不破坏 revocation; 小于
	// sseKeepaliveInterval 保证 stalled write 检测出来下一个 keepalive 才到.
	sseWriteDeadline = 10 * time.Second

	// sseChannelBuffer: per-runtime channel 缓冲. 满了 publish 跳过 (event
	// 已落 log), daemon 下次重连走 Last-Event-ID replay 补回.
	sseChannelBuffer = 16

	// sseReplayLimit: SSE 建连/重连时一次性 replay 的最大 event 数. 超出
	// 部分需要 daemon 走老路 (heartbeat 兜底 / 触发新一轮重连).
	sseReplayLimit = 1000

	// eventLogTTL: event_log row 保留时长, 跟 daemon dedup state file 24h
	// 对称. 重连超 24h 的 daemon 视为重 register, 走 heartbeat managed_bots
	// 全量 snapshot.
	eventLogTTL = 24 * time.Hour
)

// eventEnvelope 是 in-mem 推到 SSE channel 的载体. PayloadJSON 是已
// marshal 好的字符串 (dispatcher 写 event_log 时已 marshal 一次, 这里
// 直接复用避免重复 marshal).
type eventEnvelope struct {
	ID          int64
	Type        string
	PayloadJSON string
}

// sseHub 维护 runtime_id → channel 的注册表. 不持锁, 用 sync.Map.
//
// channel 永不 close (plan v6: 让 GC 回收), 防 cleanup vs publish 之间
// 的 race panic. cleanup func 只 Delete 自己注册的 channel (Load+compare
// 防止 stale cleanup 覆盖新连接).
type sseHub struct {
	channels sync.Map // runtime_id (int64) → chan eventEnvelope
}

func newSseHub() *sseHub {
	return &sseHub{}
}

// register 为指定 runtime 创建一条 channel 并注册到 hub. 如果同 runtime
// 已有 channel (老连接残留 / 同 daemon 重 register), 直接覆盖 — 老连接
// 下次 publish 时收不到, 但其自身的 ctx.Done()/TTL 会清掉自己; 这里覆盖
// 不主动通知防 race.
//
// 返回的 cleanup func 只在仍是自己的 channel 时 Delete, 避免误删后注册
// 的新 channel (defensive: 大量并发重连场景).
func (h *sseHub) register(runtimeID int64) (chan eventEnvelope, func()) {
	ch := make(chan eventEnvelope, sseChannelBuffer)
	h.channels.Store(runtimeID, ch)
	sseReconnectTotal.Add(1)
	sseActiveConns.Add(1)
	return ch, func() {
		if v, ok := h.channels.Load(runtimeID); ok {
			if existing, _ := v.(chan eventEnvelope); existing == ch {
				h.channels.Delete(runtimeID)
			}
		}
	}
}

// publish non-blocking 推到对应 runtime 的 channel. 没注册 (daemon 没连)
// 或 channel 满 (daemon 处理慢) 都跳过, 因为 caller 已把 event 写入
// event_log, daemon 重连时走 Last-Event-ID replay 自然补上.
func (h *sseHub) publish(runtimeID int64, ev eventEnvelope) {
	v, ok := h.channels.Load(runtimeID)
	if !ok {
		return
	}
	ch, _ := v.(chan eventEnvelope)
	if ch == nil {
		return
	}
	select {
	case ch <- ev:
	default:
		// channel full — daemon 处理慢/卡, event 已在 log 里, daemon 重连
		// 走 Last-Event-ID 补回. 这里不阻塞 dispatcher.
	}
}

// GET /v1/runtimes/{runtime_id}/events
//
// 长连接 handler. 流程:
//  1. authMW 已注入 (uid, space_id)
//  2. parse runtime_id
//  3. A7 ownership SQL gate (queryByID 验证 runtime 属于 caller)
//  4. parse Last-Event-ID header
//  5. 写 SSE headers + flush
//  6. 从 event_log replay missed events (>lastEventID, 最多 sseReplayLimit)
//  7. 注册 channel 入 hub
//  8. select loop: event push / 30s keepalive / 60s TTL close / ctx.Done
//
// sseEvents godoc
// @Summary      Daemon event stream (SSE)
// @Description  Long-lived Server-Sent Events stream for reverse-dispatch (upgrade / bot_provision / managed_bots_changed). Resumable via Last-Event-ID; 60s TTL forces reconnect + re-verify. Success body is text/event-stream, not JSON.
// @Tags         event
// @ID           event.stream
// @Security     Bearer
// @Param        runtime_id   path   int    true  "Runtime ID to subscribe"
// @Param        Last-Event-ID header string false "Resume from this event id"
// @Success      200 "SSE stream over text/event-stream (not a JSON body, so no content schema is declared). Connection-level errors (invalid runtime_id → 400, auth → 401/403, internal → 500) are returned as a JSON envelope.Error by middleware before the stream opens."
// @Router       /runtimes/{runtime_id}/events [get]
func (rt *Runtime) sseEvents(c *wkhttp.Context) {
	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	runtimeIDStr := c.Param("runtime_id")
	runtimeID, err := strconv.ParseInt(runtimeIDStr, 10, 64)
	if err != nil || runtimeID <= 0 {
		responseError(c, errcode.Validation)
		return
	}

	// A7 ownership SQL gate: 必须验证 runtime_id 属于 caller 的
	// (owner_uid, space_id), 否则 daemon A 持自己 api_key 传别人的
	// runtime_id 就能订阅别人的 event 流 (D1 lesson, v6 plan §3.2).
	own, err := rt.db.queryByID(runtimeID)
	if err != nil {
		rt.Error("sse: query runtime by id", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		responseError(c, errcode.InternalError)
		return
	}
	if own == nil || own.OwnerUID != ownerUID || own.SpaceID != spaceID {
		// 不区分 not found vs no permission, 防 enumeration.
		responseError(c, errcode.Forbidden)
		return
	}

	lastEventID := int64(0)
	if v := c.GetHeader("Last-Event-ID"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n >= 0 {
			lastEventID = n
		}
	}

	w := c.Writer
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// A1 nginx buffering 经典坑: 不加这个 nginx 会 buffer 整个 SSE stream
	// 直到 connection close, 完全破坏长连接语义. 跟 nginx
	// proxy_buffering off 配合 (deploy runbook).
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		// gin 默认 ResponseWriter 实现 Flusher, 走到这里说明 framework 变了.
		rt.Error("sse: response writer is not Flusher")
		return
	}

	// P1 (yujiawei R4 review): per-write SetWriteDeadline. WriteTimeout=0
	// (E5 fix) 让 server-level 不切长连接, 但 inline write/flush 在 daemon
	// stalled 时会 block 在 kernel socket Write, select/ctx 救不了.
	// ResponseController.SetWriteDeadline 通过 gin v1.9.1 ResponseWriter
	// Unwrap() chain 找到底层 *http.response, 在 block 满 sseWriteDeadline
	// 后让 Write 返 err, goroutine/fd/channel 注册及时释放, 保住 sseConnTTL
	// revocation 窗口 guarantee.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	flusher.Flush()

	// M4 fix (caster review final from codex): register 提前到 replay
	// 之前. 原顺序 (replay → register) 中间有窗口 — fleet 端 publish 看
	// 不到 channel 就 drop, daemon 这条连接只能等 60s TTL reconnect 时
	// 拿到 (heartbeat 双跑期更早, 但 Phase B 之后没 heartbeat 兜底就丢).
	//
	// 新顺序 (register → replay): replay 期间任何并发 publish 会落进
	// channel, 进 live loop 时按 id 顺序写出. daemon 端 dedup file 按
	// source_pk 去重重复推 (跟 replay 拿到的 event 撞), B1 claim 模型
	// 保证 daemon handler 只跑一次.
	//
	// P2-1 (yujiawei R4 review) cross-PR contract: register-before-replay
	// 会在 register→querySince 期间产生 dup event id (event 既被 replay 又
	// 被 live publish). correctness **不依赖 live/replay id 单调顺序**
	// (并发 INSERT/commit 可见性场景下 live id 不严格保证 > replay id),
	// 依赖:
	//   - daemon-side dedup on (event_type, source_pk) — see companion
	//     octo-daemon-cli PR's internal/sse.go dedup state (B1 claim
	//     model 保证 handler 只执行一次)
	//   - daemon last_event_id 推进只在成功处理/跳过后发生 (dedup state
	//     先 mark done, 再持久化 last_event_id)
	//
	// Phase B 前需处理 replayLimit (1000) saturation 时 live event leapfrog
	// — 当 backlog > replayLimit 时, live event 的 id 可能 > 未 replay
	// event 的 id, daemon 推 cursor 后那些未 replay 的 event 永远拿不到
	// (codex R6 N6). Phase A 双跑期 heartbeat 兜底覆盖, Phase B 必须修.
	ch, cleanup := rt.sseHub.register(runtimeID)
	defer cleanup()
	defer sseActiveConns.Add(-1)

	// 6. replay missed events 从 event_log.
	missed, qerr := rt.eventDB.querySince(runtimeID, spaceID, ownerUID, lastEventID, sseReplayLimit)
	if qerr != nil {
		rt.Warn("sse: replay query failed", zap.Error(qerr), zap.Int64("runtime_id", runtimeID))
		// 不 fatal — 进 live loop, daemon 后续会通过 keepalive/超时重连补
	} else {
		for _, ev := range missed {
			if werr := writeSSEEvent(w, rc, ev.ID, ev.EventType, ev.Payload); werr != nil {
				rt.Warn("sse: replay write failed", zap.Error(werr), zap.Int64("runtime_id", runtimeID))
				return
			}
		}
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
		flusher.Flush()
	}

	// 8. live loop
	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	// 60s TTL: align verifyCache, 强制 daemon 重连 + re-verify
	// (revocation 窗口 v3.3.6 §P1 lesson).
	ttl := time.NewTimer(sseConnTTL)
	defer ttl.Stop()

	ctx := c.Request.Context()

	for {
		select {
		case ev := <-ch:
			if werr := writeSSEEvent(w, rc, ev.ID, ev.Type, ev.PayloadJSON); werr != nil {
				rt.Warn("sse: live write failed", zap.Error(werr), zap.Int64("runtime_id", runtimeID))
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
			flusher.Flush()
		case <-keepalive.C:
			// SSE comment line (`:` 开头), client 收到丢弃, 但 keepalive
			// 保持 TCP 连接不被中间代理 idle close.
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
			if _, werr := io.WriteString(w, ": keepalive\n\n"); werr != nil {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
			flusher.Flush()
		case <-ttl.C:
			// 主动 close: daemon 收到 `event: close` 后走 reconnect 流程,
			// 重连时 authMW 再走一遍 verify (verifyCache 60s 也刚好过期),
			// banned/revoked 用户在这一刻自然被踢出.
			//
			// N4 (codex R6): close write 失败也直接 return, 不再 flush,
			// 跟 keepalive 一致 (避免 stalled write 等满 10s 后再 flush
			// 再等 10s 把 revocation 上界从 60s+10s 拉到 60s+20s).
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
			if _, werr := io.WriteString(w, "event: close\ndata: ttl-expired\n\n"); werr != nil {
				return
			}
			_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
			flusher.Flush()
			return
		case <-ctx.Done():
			return
		}
	}
}

// writeSSEEvent 把一条 event 按 SSE wire format 写入 w. 不调 flusher —
// caller 控制 batch flush.
//
// SSE 帧格式 (`text/event-stream`):
//
//	event: <type>\n
//	id: <int64>\n
//	data: <json>\n
//	\n
//
// caller 保证 payloadJSON 是合法 JSON 且不含裸 `\n` (会破坏帧边界).
// 这里依赖 JSON encoder escape 换行符的语义.
//
// P1 (yujiawei R4): 接受 rc 参数, 写前 set deadline. rc 可 nil (test 直接
// 调 writeSSEEvent 不必造 ResponseController), 但 production sseEvents
// 必传非 nil.
func writeSSEEvent(w io.Writer, rc *http.ResponseController, id int64, eventType, payloadJSON string) error {
	if rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	}
	_, err := fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", eventType, id, payloadJSON)
	return err
}

// ===== dispatcher entry points =====
//
// 每个 dispatch 内部:
//  1. marshal payload
//  2. INSERT event_log (拿自增 id)
//  3. publish 到 sseHub (non-blocking, channel 满/没连接都跳过)
//
// Phase A 双跑期间: heartbeat 仍 claimPendingXxx 兜底, 这里仅"加"路径,
// 不动 heartbeat handler. daemon 端 dedup file 去重双跑产生的重复.

func (rt *Runtime) dispatchUpgrade(runtimeID int64, spaceID, ownerUID string, task *upgradeTask) {
	payload, err := json.Marshal(map[string]any{
		"task_id":        task.ID,
		"component":      task.Component,
		"download_url":   task.DownloadURL,
		"target_version": task.ToVersion,
		"checksum":       task.Checksum,
		"metadata":       task.Metadata,
	})
	if err != nil {
		rt.Warn("sse dispatch upgrade: marshal", zap.Error(err), zap.Int64("runtime_id", runtimeID), zap.String("task_id", task.ID))
		return
	}
	id, err := rt.eventDB.insert(runtimeID, spaceID, ownerUID, eventTypeUpgrade, string(payload))
	if err != nil {
		rt.Warn("sse dispatch upgrade: event_log insert", zap.Error(err), zap.Int64("runtime_id", runtimeID), zap.String("task_id", task.ID))
		return
	}
	rt.sseHub.publish(runtimeID, eventEnvelope{ID: id, Type: eventTypeUpgrade, PayloadJSON: string(payload)})
}

// dispatchBotProvision: A3 决策 — payload 只含 command_id, 不含 bot_token.
// daemon 收 wake-up 后另起 HTTP GET /v1/bots/{bot_id}/provision
// 单独 fetch full payload, secret 永不进 SSE stream / event_log.
func (rt *Runtime) dispatchBotProvision(runtimeID int64, spaceID, ownerUID, commandID string) {
	payload, err := json.Marshal(map[string]any{"command_id": commandID})
	if err != nil {
		rt.Warn("sse dispatch bot_provision: marshal", zap.Error(err), zap.Int64("runtime_id", runtimeID), zap.String("command_id", commandID))
		return
	}
	id, err := rt.eventDB.insert(runtimeID, spaceID, ownerUID, eventTypeBotProvision, string(payload))
	if err != nil {
		rt.Warn("sse dispatch bot_provision: event_log insert", zap.Error(err), zap.Int64("runtime_id", runtimeID), zap.String("command_id", commandID))
		return
	}
	rt.sseHub.publish(runtimeID, eventEnvelope{ID: id, Type: eventTypeBotProvision, PayloadJSON: string(payload)})
}

// dispatchManagedBotsChanged: bot inventory delta (added/removed bot_uids).
// daemon 端 apply delta 到本地缓存; heartbeat snapshot 仍是 baseline source.
func (rt *Runtime) dispatchManagedBotsChanged(runtimeID int64, spaceID, ownerUID string, added, removed []string) {
	payload, err := json.Marshal(map[string]any{
		"added":   added,
		"removed": removed,
	})
	if err != nil {
		rt.Warn("sse dispatch managed_bots_changed: marshal", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		return
	}
	id, err := rt.eventDB.insert(runtimeID, spaceID, ownerUID, eventTypeManagedBotsChanged, string(payload))
	if err != nil {
		rt.Warn("sse dispatch managed_bots_changed: event_log insert", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		return
	}
	rt.sseHub.publish(runtimeID, eventEnvelope{ID: id, Type: eventTypeManagedBotsChanged, PayloadJSON: string(payload)})
}

// runEventLogSweeper 周期 prune event_log 老 row. 跑独立 goroutine (类似
// runSweeper), 失败不影响主链路.
//
// cadence 1h, TTL 24h — 不需要精确实时, 留 retention window 给 daemon
// 重连补 missed events.
func (rt *Runtime) runEventLogSweeper() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		n, err := rt.eventDB.pruneOlderThan(eventLogTTL)
		if err != nil {
			rt.Warn("event_log sweeper: prune failed", zap.Error(err))
			continue
		}
		if n > 0 {
			rt.Info("event_log sweeper pruned", zap.Int64("rows", n))
		}
	}
}
