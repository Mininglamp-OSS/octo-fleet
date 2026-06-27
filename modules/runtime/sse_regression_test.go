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
// daemon 端走 GET /v1/bots/{bot_id}/provision 单独 fetch.
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
	for _, fn := range []string{"dispatchUpgrade", "dispatchBotProvision", "dispatchManagedBotsChanged"} {
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

// runtime 绑定 (Jerry-Xin fleet#44 blocking): api_key 只绑 (owner, space),
// fleet 区分不出 caller 是哪台 daemon. 只验 owner+space 的话, 同 owner+space
// 下 daemon A 能拿别的 runtime 的 bot.id 来 fetch + claim + ack, 绕过 heartbeat
// 路径 (claimPendingBotProvision) 用 daemon_id 保证的路由. fetch 必须:
//  1. 要求 daemon 自报 runtime_id 并验它归 caller 所有 (同 sseEvents A7 gate)
//  2. 校验 bot.RuntimeID == runtime_id (这个 bot 确实归来取的这台 daemon)
func TestSSERegression_FetchBotProvisionRuntimeBinding(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "bot_provision_fetch.go"), "fetchBotProvision")
	// 解析并校验 caller 自报的 runtime_id.
	if !strings.Contains(body, `c.Query("runtime_id")`) {
		t.Error("fetchBotProvision 必须从请求读 runtime_id — api_key 只绑 owner+space, 路由绑定要靠 daemon 自报 runtime_id (fleet#44)")
	}
	// runtime 归属 gate (同 sseEvents): queryByID + owner/space 验拥有关系.
	if !strings.Contains(body, "queryByID(runtimeID)") {
		t.Error("fetchBotProvision 必须 queryByID(runtimeID) 验 runtime 归 caller 所有 (A7 ownership gate, fleet#44)")
	}
	// bot 必须归这台 runtime, 否则就是跨 daemon 劫持.
	if !strings.Contains(body, "row.RuntimeID != runtimeID") {
		t.Error("fetchBotProvision 必须校验 row.RuntimeID == runtimeID — 防同 owner+space 下跨 daemon 拿别人 bot provision (fleet#44)")
	}
}

// claim UPDATE 必须带 runtime_id 约束, 跟 claimPendingBotProvision 的 daemon_id
// 路由绑定对齐 — 仅靠 handler 顶层校验不够, claim 这步是真正 mint claim_token
// 的地方, query→update 之间有窗口, 正确性不能只依赖前置 4xx 校验.
func TestSSERegression_FetchBotProvisionClaimBindsRuntime(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "bot_provision_fetch.go"), "respondBotProvisionByStatus")
	if !strings.Contains(body, "WHERE id=? AND status=? AND runtime_id=?") {
		t.Errorf("respondBotProvisionByStatus 的 claim UPDATE 必须 gate 在完整谓词 "+
			"`WHERE id=? AND status=? AND runtime_id=?` 上, 让跨 runtime 的 claim 匹配 0 行 (fleet#44).\n\nbody:\n%s", body)
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
	for _, fn := range []string{"claimPendingUpgrade", "claimPendingBotProvision"} {
		if !strings.Contains(body, fn+"(") {
			t.Errorf("heartbeat 必须仍调 %s — Phase A 双跑兜底 (v6 plan §4). 拆这个等 Phase B PR.", fn)
		}
	}
}

// SSE 路由必须经 daemon authMW (api_key Bearer). 没接进 daemon scope group
// 就裸暴露了 long-lived endpoint 给任意 caller. (URL 资源化后路径无 daemon
// 段,daemon/web 靠 scope 区分——见 api.go Route 注释。)
func TestSSERegression_RoutesUnderDaemonAuthGroup(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "api.go"), "Route")
	// daemon scope group 起始到下一个 group (web) 之间的片段必须含 SSE +
	// bot provision endpoint.
	daemonGroupStart := strings.Index(body, `daemon := r.Group("/v1", auth.Middleware("daemon"))`)
	if daemonGroupStart < 0 {
		t.Fatal("Route 找不到 daemon scope group (auth middleware 已被改？)")
	}
	webIdx := strings.Index(body[daemonGroupStart:], "web := r.Group")
	if webIdx < 0 {
		t.Fatal("Route 找不到 daemon group 结束 marker (web group)")
	}
	daemonBlock := body[daemonGroupStart : daemonGroupStart+webIdx]
	if !strings.Contains(daemonBlock, `GET("/runtimes/:runtime_id/events"`) {
		t.Error("GET /runtimes/:runtime_id/events 必须在 daemon authMW group 内")
	}
	if !strings.Contains(daemonBlock, `GET("/bots/:bot_id/provision"`) {
		t.Error("GET /bots/:bot_id/provision 必须在 daemon authMW group 内")
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

// Phase B metrics (#36): 三个 counter/gauge 必须存在且在正确路径上更新.
// source-grep 锁不变量: future refactor 删 metrics 或放错路径时立刻 fail.
//
// 指标语义 (v6 plan §4 E7 Phase B go/no-go gate):
//   - sse_active_conns:           gauge,   register +1 / handler exit -1
//   - sse_reconnect_total:        counter, register +1
//   - heartbeat_fallback_hit_total: counter, heartbeat claim pending +1

func TestSSERegression_MetricsVarDeclarations(t *testing.T) {
	src := mustReadSource(t, "metrics.go")
	for _, name := range []string{"sse_active_conns", "sse_reconnect_total", "heartbeat_fallback_hit_total"} {
		if !strings.Contains(src, name) {
			t.Errorf("metrics.go 必须声明 expvar %q (Phase B go/no-go gate, #36)", name)
		}
	}
}

func TestSSERegression_RegisterIncrementsMetrics(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "sse.go"), "register")
	if !strings.Contains(body, "sseReconnectTotal.Add(1)") {
		t.Error("sseHub.register 必须 sseReconnectTotal.Add(1) — 每次新 SSE 连接 +1 (#36)")
	}
	if !strings.Contains(body, "sseActiveConns.Add(1)") {
		t.Error("sseHub.register 必须 sseActiveConns.Add(1) — 新连接进入 active gauge (#36)")
	}
}

func TestSSERegression_SSEEventsDecrementsActiveGauge(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "sse.go"), "sseEvents")
	// defer sseActiveConns.Add(-1) 必须在 handler 内, 跟 defer cleanup() 配对.
	// 不设的话 active gauge 永远涨, Phase B 数据基线全废.
	if !strings.Contains(body, "sseActiveConns.Add(-1)") {
		t.Error("sseEvents 必须 defer sseActiveConns.Add(-1) — handler 退出时 -1 保持 gauge 正确 (#36)")
	}
}

func TestSSERegression_HeartbeatClaimsIncrementFallbackCounter(t *testing.T) {
	body := extractFuncBody(t, mustReadSource(t, "api.go"), "heartbeat")
	// 两个 claim 路径各 +1 (upgrade + bot provision). heartbeat 是 SSE 的 fallback,
	// 每次 claim 说明 SSE 没提前送到, 计一次 fallback hit.
	if !strings.Contains(body, "heartbeatFallbackHitTotal.Add(1)") {
		t.Error("heartbeat 必须在 claimPending 路径 increment heartbeatFallbackHitTotal (#36: Phase B 需该数据判断 SSE 是否够稳)")
	}
}
