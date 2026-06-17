package runtime

import (
	"os"
	"regexp"
	"testing"
)

// 决策三 v6 plan §1.4 D8 / E5 regression:
//
// SSE 是长连接 (默认 90+ 秒 keepalive, 60s TTL re-verify 后立即重连).
// 如果 fleet root main.go 给 http.Server 设非零 WriteTimeout, 任何超过
// 该 deadline 的 SSE conn 会被 net/http 主动 close, daemon 端的体感:
//   - 写到一半的 event 帧被截断
//   - daemon 收到 unexpected EOF, exp backoff 重连, 看起来像 flapping
//   - dispatched event 因为 truncated 帧 daemon parse 失败 → in-mem 丢
//     (event_log 里还在, 但 daemon 端 Last-Event-ID 没更新, 重连 replay
//     OK; 不过会持续 5-10s 一轮 churn, 完全破坏 SSE 体验)
//
// 这个 test 一直 guard: 不让未来 PR 给 main.go 加 WriteTimeout: <非零>.
// 如果以后真要分 endpoint 加 timeout (比如 admin /metrics 加快超时),
// 必须 per-handler 控制 (http.TimeoutHandler 包不包括 SSE handler 等),
// 不能写 server-level 全局.
//
// P2-4 caveat (yujiawei R4 review): 这个 test 的真层不是 fleet/main.go,
// 而是 octo-lib `server.Server.Run` → `wkhttp.WKHttp.Run` → 内部
// `gin.Engine.Run` → 默认 http.Server (空 WriteTimeout = 0). fleet main.go
// 当前不直接构造 http.Server, 所以这里 grep main.go 是 **defensive layer
// (catches the case where fleet starts overriding the lib path)**, 不是
// 真 layer.
//
// **真 invariant 在 octo-lib**: 如果未来 octo-lib bump 在 `WKHttp.Run` /
// `Server.Run` 内部 wrap http.Server 加 WriteTimeout, fleet 这条 test 不
// 会捕获, SSE 会被切断. 真要 enforce 应该在 octo-lib 端独立 test (本仓库
// 无法跨 repo grep). 见 P2-4 follow-up issue.
//
// 当前 octo-lib v0.0.0-20260515014003-2cdafe082b88: `server.Server.Run`
// 调 `s.r.Run(addr...)` 即 `wkhttp.WKHttp.Run` (本质 gin Engine.Run),
// gin v1.9.1 Engine.Run 创 http.Server 不设 WriteTimeout (= 0), 当前
// invariant 满足.
func TestFleetMain_NoWriteTimeoutLiteral(t *testing.T) {
	// 路径 fleet 根 main.go (modules/runtime → root 跳 2 层). 如果路径变了,
	// 这个 fatal 强制人来更新 — 防 silent 跳过 (跟 D8 lesson 同形).
	src, err := os.ReadFile("../../main.go")
	if err != nil {
		t.Fatalf("can't read fleet main.go (path moved? update test or 在新位置加 WriteTimeout check): %v", err)
	}
	// 匹配 `WriteTimeout:` 紧跟数字非零 (包括 5*time.Second 这种).
	// 写 `WriteTimeout: 0` 是显式 zero 不算违规.
	if regexp.MustCompile(`WriteTimeout:\s*[1-9]`).Match(src) {
		t.Error("fleet main.go 含非零 WriteTimeout 字面值 — SSE 长连接会被 net/http 切断 (v6 plan §1.4 D8 / E5 regression). 必须 per-handler timeout 不能 server-level 全局.")
	}
}
