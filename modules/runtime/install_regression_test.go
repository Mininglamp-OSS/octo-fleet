package runtime

import (
	"regexp"
	"testing"
)

// octo(openclaw 的 octo 适配插件)未安装时,createPluginUpgradeTask 必须按 install
// 受理(不无条件 400);但 cc-octo 未安装仍须 400(其安装需用户提供 LLM 网关/key 等
// 额外配置,另行支持)。锁住"空版本守卫已 scope 到非 componentPlugin",防退化成
// blanket 400(阻断 octo 一键安装)或 blanket 放行(误开 cc-octo install)。
func TestCreatePluginUpgradeTask_InstallScopedToOcto(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "createPluginUpgradeTask")

	scoped := regexp.MustCompile(`fromVersion\s*==\s*""\s*&&\s*component\s*!=\s*componentPlugin`)
	if !scoped.MatchString(body) {
		t.Fatalf("createPluginUpgradeTask 的空版本守卫未 scope 到 componentPlugin —— 要么 blanket 400 阻断 octo 安装,要么 blanket 放行误开 cc-octo install")
	}
}

// install 复用 upgrade 的版本比对:空 fromVersion 必须被判为"比 latest 旧"才能放行
// install(否则版本比对会把首装拦成 Conflict)。锁住这条依赖。
func TestIsVersionOlder_EmptyIsOlderThanLatest(t *testing.T) {
	if !isVersionOlder("", "0.7.0") {
		t.Fatal(`isVersionOlder("", "0.7.0") 应为 true —— 否则未安装(空版本)的 install 会被版本比对拦成 Conflict`)
	}
}
