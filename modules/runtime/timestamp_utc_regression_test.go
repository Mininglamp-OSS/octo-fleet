package runtime

import (
	"os"
	"regexp"
	"testing"
)

// Test_Issue67_TimestampsAreUTC guards the fix for issue #67.
//
// 现象: octo-web Runtimes 页「最近活跃」在 MySQL 会话时区非 UTC 的部署上恒显 `—`。
//
// 根因: db.go 把 agent_runtime.last_seen_at / device.last_seen_at /
// device_component.reported_at 用 MySQL 的 session-local 时间函数写入(返回值取决于
// 会话时区,默认继承容器/主机 system_time_zone)。驱动侧 DSN 无 loc=/time_zone=,
// go-sql-driver 按 UTC 解析 DATETIME(parseTime=true),api.go 又把它当 naive UTC 串
// 返回;前端硬加 Z 解析。于是会话时区为 UTC+8 时,wall-clock 比真实 UTC 快 8h,
// 前端算出「未来」时刻 → 年龄差为负 → humanizeAge 回退 `—`。
//
// 修复: db.go 内所有时间戳写入与过期比较一律改用 UTC_TIMESTAMP()(与会话时区无关,
// 恒为 UTC),写入与 markStaleOffline 比较保持同一 UTC 基准。
//
// 本仓库无 sqlmock+httptest harness(见 sse_test.go / owner_regression_test.go 头部
// 论证),且该 bug 是 SQL 层的时区选择,只有在非 UTC 会话的真实 MySQL 上才能行为级
// 复现,故沿用 write_timeout_regression_test.go 的源码不变量(source-grep)风格做回归
// 守卫:db.go 不得再出现 session-tz-dependent 的 NOW(),时间戳必须用 UTC_TIMESTAMP()。
func Test_Issue67_TimestampsAreUTC(t *testing.T) {
	src, err := os.ReadFile("db.go")
	if err != nil {
		t.Fatalf("can't read db.go (path moved? update test or 在新位置加 UTC 检查): %v", err)
	}

	// 去掉行注释,避免后续在注释里提到 NOW() 触发误判(SQL 串内无 // 序列)。
	code := regexp.MustCompile(`(?m)//.*$`).ReplaceAll(src, nil)

	// NOW() 返回会话时区本地时间;UTC_TIMESTAMP() 恒为 UTC。db.go 的全部时间戳
	// 写入/比较都是 last_seen_at / reported_at,必须与会话时区解耦。
	if loc := regexp.MustCompile(`\bNOW\s*\(\s*\)`).FindIndex(code); loc != nil {
		t.Errorf("modules/runtime/db.go 仍使用 session-tz-dependent NOW()(offset %d)— issue #67: "+
			"last_seen_at/reported_at 必须用 UTC_TIMESTAMP() 写入与比较,否则非 UTC 部署「最近活跃」恒显 —", loc[0])
	}
	if !regexp.MustCompile(`\bUTC_TIMESTAMP\s*\(\s*\)`).Match(code) {
		t.Error("modules/runtime/db.go 未使用 UTC_TIMESTAMP() 写时间戳(issue #67 修复点)")
	}
}
