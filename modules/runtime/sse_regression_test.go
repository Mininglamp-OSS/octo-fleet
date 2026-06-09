package runtime

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 决策三 SSE source-grep regression. 套用 owner_regression_test.go 同
// pattern (头部论证: fleet 暂无 DB 测试 harness, source-grep 是低成本
// 高价值的"防 future refactor 把 invariant 拆掉"网).
//
// 如果你故意改了下面任何一条 invariant, 你必须**改这个测试**并在 PR
// 描述里写为啥这个 invariant 不再需要 — 这正是这些测试的目的.

// A7 ownership SQL gate: sseEvents 必须验 runtime 属于 caller 的
// (owner_uid, space_id), 否则 daemon A 持自己 api_key 传别人的 runtime_id
// 就能订阅别人的 event 流 (D1 lesson).
func TestSSERegression_OwnershipGate(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "sse.go"), "sseEvents")
	// 必须 query 拿到 row 后 compare owner_uid + space_id.
	if !strings.Contains(body, "queryByID(runtimeID)") {
		t.Error("sseEvents 必须先 queryByID(runtimeID) 拿 row 才能校 ownership (A7 v6 plan §3.2)")
	}
	if !strings.Contains(body, "OwnerUID != ownerUID") {
		t.Error("sseEvents 必须 compare row.OwnerUID != ownerUID (A7 ownership gate)")
	}
	if !strings.Contains(body, "SpaceID != spaceID") {
		t.Error("sseEvents 必须 compare row.SpaceID != spaceID (A7 ownership gate)")
	}
}

// A3 secret-not-in-stream: dispatchBotProvision payload 只能含 command_id,
// 不能含 bot_token / claim_token / workspace_id 等 secret/敏感字段.
// daemon 端走 GET /v1/daemon/bot-provisions/:id 单独 fetch.
func TestSSERegression_BotProvisionPayloadHasNoSecret(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "sse.go"), "dispatchBotProvision")
	forbidden := []string{"bot_token", "claim_token", "workspace_id", "BotToken", "ClaimToken", "WorkspaceID"}
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Errorf("dispatchBotProvision payload 不能含 %q — A3 secret-not-in-stream (v6 plan §3.3 §3.4)", f)
		}
	}
	if !strings.Contains(body, "command_id") {
		t.Error("dispatchBotProvision payload 必须含 command_id (daemon 走 fetch endpoint 拿 full payload)")
	}
}

// dispatcher 必须先 INSERT event_log 再 publish — 否则 daemon 重连
// 走 Last-Event-ID replay 拿不到这条 event (in-mem 推丢了, 持久化层无记录).
func TestSSERegression_DispatchPersistsBeforePublish(t *testing.T) {
	src := mustReadSource(t, "sse.go")
	for _, fn := range []string{"dispatchPing", "dispatchUpgrade", "dispatchBotProvision", "dispatchManagedBotsChanged"} {
		body := extractFuncBody(t, src, fn)
		insertIdx := strings.Index(body, "eventDB.insert(")
		publishIdx := strings.Index(body, "sseHub.publish(")
		if insertIdx < 0 {
			t.Errorf("%s must call eventDB.insert() to persist event_log", fn)
			continue
		}
		if publishIdx < 0 {
			t.Errorf("%s must call sseHub.publish() to push in-mem", fn)
			continue
		}
		if insertIdx > publishIdx {
			t.Errorf("%s must INSERT event_log BEFORE publish — otherwise reconnect Last-Event-ID replay loses this event", fn)
		}
	}
}

// SSE handler 必须设 X-Accel-Buffering: no — A1 nginx buffering 经典坑,
// 不设的话 nginx 会 buffer 整个 stream 等 connection close, 长连接语义
// 完全坏掉.
func TestSSERegression_XAccelBufferingHeader(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "sse.go"), "sseEvents")
	if !strings.Contains(body, "X-Accel-Buffering") {
		t.Error("sseEvents 必须 set header X-Accel-Buffering: no (A1 nginx buffering 坑, v6 plan §3.2)")
	}
	if !strings.Contains(body, `"no"`) || !regexp.MustCompile(`X-Accel-Buffering"\s*,\s*"no"`).MatchString(body) {
		t.Error("X-Accel-Buffering 必须设为 \"no\"")
	}
}

// bot_provision fetch endpoint 必须做 ownership SQL gate (同 A7) —
// 防 daemon A 持自己 api_key 拿别人 bot.id 就能 fetch 别人的 bot_token.
func TestSSERegression_FetchBotProvisionOwnershipGate(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "bot_provision_fetch.go"), "fetchBotProvision")
	if !strings.Contains(body, "OwnerUID != ownerUID") || !strings.Contains(body, "SpaceID != spaceID") {
		t.Error("fetchBotProvision 必须 compare row.OwnerUID/SpaceID 跟 caller (A7 ownership gate, secret 防泄露)")
	}
}

// Phase A 双跑保证: heartbeat handler 不能拆掉 claimPendingXxx 调用,
// 否则 SSE 没工作时反向派发完全断 (新 daemon 还没建 SSE 之前的 1-2s
// window / SSE 重连之间的 gap / SSE channel full drop 等场景都失守).
//
// Phase B PR 才能拆这些, 那一刻 daemon 端必须保证 SSE 100% 工作 ≥ 1 周
// 观察期 (v6 plan §4 + §E7).
func TestSSERegression_HeartbeatStillDispatchesPendingInPhaseA(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "api.go"), "heartbeat")
	for _, fn := range []string{"claimPendingPing", "claimPendingUpgrade", "claimPendingBotProvision"} {
		if !strings.Contains(body, fn+"(") {
			t.Errorf("heartbeat 必须仍调 %s — Phase A 双跑兜底 (v6 plan §4). 拆这个等 Phase B PR.", fn)
		}
	}
}

// SSE 路由必须经 daemon authMW (api_key Bearer). 没接进 r.Group("/v1/daemon")
// 就裸暴露了 long-lived endpoint 给任意 caller.
func TestSSERegression_RoutesUnderDaemonAuthGroup(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "api.go"), "Route")
	// "GET("/events"" 必须在 daemon group block 内. 用粗 heuristic:
	// daemon group 起始到结束的字符片段必须含两个新 endpoint.
	daemonGroupStart := strings.Index(body, `daemon := r.Group("/v1/daemon"`)
	if daemonGroupStart < 0 {
		t.Fatal("Route 找不到 daemon group (auth middleware 已被改？)")
	}
	internalIdx := strings.Index(body[daemonGroupStart:], "internal := r.Group")
	if internalIdx < 0 {
		t.Fatal("Route 找不到 daemon group 结束 marker")
	}
	daemonBlock := body[daemonGroupStart : daemonGroupStart+internalIdx]
	if !strings.Contains(daemonBlock, `GET("/events"`) {
		t.Error("GET /events 必须在 daemon authMW group 内")
	}
	if !strings.Contains(daemonBlock, `GET("/bot-provisions/`) {
		t.Error("GET /bot-provisions/:command_id 必须在 daemon authMW group 内")
	}
}

// G13 (caster review fleet round 1): pruneOlderThan 必须 LIMIT batch + for
// loop 分批删, 防 1M+ row 时一次 DELETE 锁表几十秒 + binlog 单 statement
// 暴涨 (plan v6 §3.4 explicit). source-grep 锁住 future refactor 不要
// 改回 unbounded DELETE.
func TestSSERegression_PruneBatchLimit(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "event_log.go"), "pruneOlderThan")
	// dbr 方法链可能跨行 (.\n\tLimit(...)), 用 regex 容忍空白.
	if !regexp.MustCompile(`Limit\s*\(`).MatchString(body) {
		t.Error("pruneOlderThan 必须 Limit(batchSize) 分批 (G13: 防长事务锁表/binlog 暴涨, plan v6 §3.4)")
	}
	if !strings.Contains(body, "for {") && !strings.Contains(body, "for ;") {
		t.Error("pruneOlderThan 必须 for loop 分批, 否则单次 DELETE 仍可能只清掉前 batchSize 行 (G13)")
	}
	if !strings.Contains(body, "RowsAffected") {
		t.Error("pruneOlderThan 必须 RowsAffected() 判断 break 条件 (G13: 否则不知何时 batch 完)")
	}
}

// F3 (caster review push 前 final): firstRuntimeIDForDaemon 必须 status=
// 'online' 过滤, 否则最老 runtime offline 时 SSE 推空 channel 静默丢
// (heartbeat 5-7s 兜底但失去 SSE 加速). source-grep 锁住 future refactor
// 不要拿掉 filter.
func TestSSERegression_FirstRuntimeFilterOnline(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "db.go"), "firstRuntimeIDForDaemon")
	if !strings.Contains(body, "status='online'") && !strings.Contains(body, `status="online"`) {
		t.Error("firstRuntimeIDForDaemon 必须 SQL 含 status='online' 过滤 (F3: 防 SSE 推到 offline runtime 静默丢, plan §3.5)")
	}
}

// R3-1 (codex round 3 BLOCKER): SSE fast-path 跳过 heartbeat 的 claim
// (status pending→dispatched), 所以 daemon 经 SSE 直接 ReportPing 时 row
// 仍是 'pending'. updatePingResult SQL 必须接受 pending|dispatched 两种
// 起始状态, 否则 SSE 路径 affected=0 静默成功, daemon dedup mark + advance,
// heartbeat 后续 claim 后 daemon dedup block, ping 永远卡到 timeoutPing.
func TestSSERegression_UpdatePingResultAcceptsPending(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "db.go"), "updatePingResult")
	// SQL 必须含 status IN (..., 'pending', ...) 而非只 status='dispatched'
	if !regexp.MustCompile(`status\s+IN\s*\([^)]*'pending'[^)]*\)`).MatchString(body) {
		t.Error("updatePingResult SQL 必须 status IN ('pending','dispatched') 不能只 'dispatched' — SSE 路径不 claim, row 仍是 pending (R3 BLOCKER)")
	}
}

// R3-2 (codex round 3 BLOCKER): upgrade validTransitionsFrom 第一跳必须
// 允许 'pending' 起跳 (同 R3-1 理由 for upgrade task).
//
// 表驱动直接调 validTransitionsFrom — 避免 source-grep 跨 case 误匹配
// (codex round 3 MINOR: 老 regex 可能匹配下面的 failed case 而漏掉
// downloading/installing 真改回严格语义的 regression).
func TestSSERegression_UpgradeFirstTransitionAllowsPending(t *testing.T) {
	cases := []struct {
		component string
		target    string
	}{
		{"octo-daemon", "downloading"}, // daemon-level 第一跳
		{"octo", "installing"},         // plugin 第一跳
		{"claude", "installing"},       // provider 第一跳
		{"codex", "installing"},
		{"openclaw", "installing"},
		{"hermes", "installing"},
	}
	for _, tc := range cases {
		t.Run(tc.component+"_"+tc.target, func(t *testing.T) {
			allowed := validTransitionsFrom(tc.component, tc.target)
			found := false
			for _, s := range allowed {
				if s == "pending" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("validTransitionsFrom(%q, %q) 必须含 'pending' 起跳 (R3 BLOCKER: SSE 不 claim, row 是 pending), got %v",
					tc.component, tc.target, allowed)
			}
		})
	}
}

// F-1 (lml2468 review): insertUpgradeTask 必须接 tx.Commit() + LoadOne err.
// 之前 swallow tx.Commit 会让 daemon 收到不存在 taskID 的 SSE event; swallow
// LoadOne 会绕过 "已 in-progress" 检查重复 INSERT pending row.
func TestSSERegression_InsertUpgradeTaskChecksErrors(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "upgrade.go"), "insertUpgradeTask")
	// tx.Commit() 返回值必须被 check (不能裸 `tx.Commit()` 不接)
	if !regexp.MustCompile(`if\s+err\s*:=\s*tx\.Commit\(\)\s*;\s*err\s*!=\s*nil`).MatchString(body) {
		t.Error("insertUpgradeTask 必须 `if err := tx.Commit(); err != nil` 接 Commit err (F-1: 防 200 OK 但 row 不存在)")
	}
	// LoadOne 返回值必须被 check (不能裸 `.LoadOne(&v)` 不接)
	if !regexp.MustCompile(`if\s+err\s*:=\s*[^;]*LoadOne\(`).MatchString(body) {
		t.Error("insertUpgradeTask 必须 `if err := ....LoadOne(...); err != nil` 接 LoadOne err (F-1: 防 activeCount=0 重复 pending)")
	}
}

// F-3 (lml2468 review): completeUpgradeIfMatched + WithRuntime SQL WHERE
// status IN 必须含 'pending'. R3 first-hop 接 pending 后, close-out 也要接 —
// daemon crash → restart → register 触发 close-out 时 row 可能仍 pending.
func TestSSERegression_CompleteUpgradeAcceptsPending(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	for _, fn := range []string{"completeUpgradeIfMatched", "completeUpgradeIfMatchedWithRuntime"} {
		body := extractFuncBody(t, src, fn)
		// status IN ('pending', ...) — pending 必须在 list 里
		if !regexp.MustCompile(`status\s+IN\s*\([^)]*'pending'[^)]*\)`).MatchString(body) {
			t.Errorf("%s SQL WHERE status IN 必须含 'pending' (F-3 对称 R3: SSE 不 claim, close-out 时 row 可能仍 pending)", fn)
		}
	}
}

// F-4 (lml2468 review): fetchBotProvision 不复用 buildPendingBotProvision
// (含 bot_token 字段, 今天 m.BotToken="" 不漏). 改用 explicit whitelist
// botProvisionFetchResponse {BotUID, WorkspaceID, ClaimToken}, 防 future
// 给 bot 表加 token cache 列时 buildPendingBotProvision 自动 include leak.
func TestSSERegression_BotProvisionFetchPayloadHasNoSecret(t *testing.T) {
	src := mustReadSource(t, "bot_provision_fetch.go")
	body := extractFuncBody(t, src, "respondBotProvisionByStatus")
	// 不能调 buildPendingBotProvision (它含 bot_token 字段)
	if strings.Contains(body, "buildPendingBotProvision") {
		t.Error("respondBotProvisionByStatus 不能复用 buildPendingBotProvision — 后者含 bot_token 字段 (F-4: 防 future 给 bot 表加 token cache 列时静默 leak)")
	}
	// botProvisionFetchResponse struct 必须存在且显式列字段
	if !regexp.MustCompile(`type\s+botProvisionFetchResponse\s+struct`).MatchString(src) {
		t.Error("必须有 botProvisionFetchResponse struct (F-4: explicit whitelist)")
	}
	// struct 不能含 bot_token 字段
	structRe := regexp.MustCompile(`(?s)type\s+botProvisionFetchResponse\s+struct\s*\{[^}]*\}`)
	structDef := structRe.FindString(src)
	if structDef == "" {
		t.Fatal("can't extract botProvisionFetchResponse struct")
	}
	if strings.Contains(structDef, "BotToken") || strings.Contains(structDef, "bot_token") {
		t.Errorf("botProvisionFetchResponse 不能含 bot_token 字段 (F-4): got %s", structDef)
	}
	// 必须显式有 BotUID / WorkspaceID / ClaimToken
	for _, field := range []string{"BotUID", "WorkspaceID", "ClaimToken"} {
		if !strings.Contains(structDef, field) {
			t.Errorf("botProvisionFetchResponse 必须含 %s 字段", field)
		}
	}
}

// F-2 (lml2468 review): ackBot fail path (status='failed') 必须 publish
// managed_bots_changed{removed} compensating event. patchBotMint 已发 added
// (bot_minted → SSE delta), 这里不发 removed 会让 daemon 缓存 hold phantom
// bot 直到 heartbeat snapshot 5-7s 后才 reconcile.
func TestSSERegression_AckBotFailPublishesRemoval(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "bot.go"), "ackBot")
	// failed status 路径必须调 dispatchManagedBotsChanged with removed
	// (具体 regex: 'botStatusFailed' 跟 dispatchManagedBotsChanged 在同一 if 内)
	if !regexp.MustCompile(`(?s)botStatusFailed[^{]*\{[^}]*dispatchManagedBotsChanged`).MatchString(body) {
		t.Error("ackBot 在 req.Status == botStatusFailed 路径必须 dispatchManagedBotsChanged with removed (F-2: 补 daemon 缓存清 phantom bot)")
	}
}

// P1 (yujiawei R4 review): SSE inline write/flush 必须 per-write set
// SetWriteDeadline. WriteTimeout=0 (E5 fix) 让 server-level 不切长连接,
// 但 inline write 在 daemon stalled-but-open 时会 block 在 kernel socket
// Write, select/ctx 救不了. 没 deadline → goroutine/fd/sseHub channel
// 注册 leak 到 OS TCP send timeout (几分钟), 60s revocation 窗口被拉到
// 不可预测, 削弱本 PR 自己声明的 security guarantee.
//
// 这个 test 锁 4 个不变性:
//  1. sseWriteDeadline const 存在 + 数值 < sseKeepaliveInterval
//     (保证 stalled write 检测出来下一个 keepalive 才到, 不冲突)
//  2. writeSSEEvent signature 必须含 rc *http.ResponseController 参数
//  3. sseEvents 函数体含 http.NewResponseController(w) (handler 顶层创 rc)
//  4. sseEvents 函数体 ≥ 5 处 SetWriteDeadline (initial / replay /
//     live / keepalive / close 各处 inline write 前)
func TestSSERegression_SSEWritesHaveDeadline(t *testing.T) {
	src := mustReadSource(t, "sse.go")

	// 1. const 存在 + 数值 < sseKeepaliveInterval (动态 grep keepalive 值,
	//    不 hardcode 30 — cc N2: future 改 sseKeepaliveInterval=5s 但 deadline
	//    留 10s 会破不变性, hardcode test 不会 catch).
	deadlineRe := regexp.MustCompile(`sseWriteDeadline\s*=\s*(\d+)\s*\*\s*time\.Second`)
	dm := deadlineRe.FindStringSubmatch(src)
	if dm == nil {
		t.Fatal("sse.go 必须定义 const sseWriteDeadline = <N> * time.Second (P1 yujiawei R4 fix)")
	}
	deadlineN, err := strconv.Atoi(dm[1])
	if err != nil {
		t.Fatalf("can't parse sseWriteDeadline value %q: %v", dm[1], err)
	}

	keepRe := regexp.MustCompile(`sseKeepaliveInterval\s*=\s*(\d+)\s*\*\s*time\.Second`)
	km := keepRe.FindStringSubmatch(src)
	if km == nil {
		t.Fatal("can't find sseKeepaliveInterval const (test 假设它存在)")
	}
	keepN, err := strconv.Atoi(km[1])
	if err != nil {
		t.Fatalf("can't parse sseKeepaliveInterval value %q: %v", km[1], err)
	}

	// cc N1 + codex N5: 必须 numeric 比较, 不能 string 比. "100" >= "30"
	// 字典序是 false (首字符 '1' < '3'), 100s 这种值会绕过 string 比.
	if deadlineN >= keepN {
		t.Errorf("sseWriteDeadline=%ds 必须 < sseKeepaliveInterval=%ds 保证 stalled write 在下一个 keepalive 前检测出来 (P1 yujiawei R4)", deadlineN, keepN)
	}

	// 2. writeSSEEvent signature 含 rc *http.ResponseController
	if !regexp.MustCompile(`func\s+writeSSEEvent\s*\([^)]*rc\s+\*http\.ResponseController[^)]*\)`).MatchString(src) {
		t.Error("writeSSEEvent signature 必须含 rc *http.ResponseController 参数 (P1 yujiawei R4 fix)")
	}

	// 3 + 4. sseEvents 函数体
	body := extractFuncBody(t, src, "sseEvents")
	if !strings.Contains(body, "http.NewResponseController(w)") {
		t.Error("sseEvents 必须创 rc := http.NewResponseController(w) 用于 per-write SetWriteDeadline (P1 yujiawei R4)")
	}
	// 数 SetWriteDeadline 调用次数:
	//   - initial flush (Flush 前) — 1
	//   - replay flush (Flush 前) — 1
	//   - live case Flush 前 — 1
	//   - keepalive: 1 (WriteString 前) + 1 (Flush 前) — 2
	//   - close: 1 (WriteString 前) + 1 (Flush 前) — 2
	// 总 = 7 处. 设阈 ≥ 5 留 buffer 给 future refactor.
	setCount := strings.Count(body, "SetWriteDeadline(")
	if setCount < 5 {
		t.Errorf("sseEvents 必须 ≥ 5 处 SetWriteDeadline (initial / replay / live / keepalive write+flush / close write+flush), got %d (P1 yujiawei R4 fix)", setCount)
	}
}
