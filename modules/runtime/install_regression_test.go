package runtime

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// octo(openclaw 的 octo 适配插件)未安装时,createPluginUpgradeTask 必须按 install
// 受理(不无条件 400);cc-octo 未安装需用户提供 LLM 网关/key 才放行。锁住"空版本守卫
// 已 switch 到 per-component install",防退化 blanket 400 或 blanket 放行。
func TestCreatePluginUpgradeTask_InstallScopedToOcto(t *testing.T) {
	src, err := os.ReadFile("upgrade.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// 旧的一刀切 guard 必须已移除。
	if strings.Contains(body, `fromVersion == "" && component != componentPlugin`) {
		t.Error("old blanket install guard still present; must be the per-component install switch")
	}
	// 新 guard:install 分流必须显式处理 componentPlugin 与 componentCcOcto。
	if !strings.Contains(body, "isInstall := fromVersion ==") {
		t.Error("install must be detected via empty fromVersion (isInstall)")
	}
	if !regexp.MustCompile(`case componentCcOcto:`).MatchString(body) {
		t.Error("install switch must handle componentCcOcto (needs secret) explicitly")
	}
}

// cc-octo install 必须放开空 fromVersion，且 secret 经 insertTaskArgs.CcSecret 传入,
// insertUpgradeTask 在 SSE dispatch 之前存入 store，绝不进 metadata。
func TestCreatePluginUpgradeTask_CcOctoInstallNeedsSecret(t *testing.T) {
	src, err := os.ReadFile("upgrade.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)

	// cc-octo install 缺 secret 必须拒绝(校验 GatewayURL/APIKey 非空)。
	if !regexp.MustCompile(`req\.GatewayURL == "" \|\| req\.APIKey == ""`).MatchString(body) {
		t.Error("cc-octo install must reject empty gateway_url/api_key")
	}
	// secret 通过 insertTaskArgs.CcSecret 传入(而非 createPluginUpgradeTask 末尾
	// 在 insertUpgradeTask 返回后再 put —— 那样 SSE 已先 dispatch，存在 fetch 404 竞态)。
	if !strings.Contains(body, "CcSecret:") {
		t.Error("createPluginUpgradeTask must pass the secret via insertTaskArgs.CcSecret")
	}
	// put 必须发生在 dispatchUpgrade 之前(同一函数内,put 文本先于 dispatchUpgrade 文本)。
	putIdx := strings.Index(body, "rt.ccSecrets.put(")
	dispIdx := strings.Index(body, "rt.dispatchUpgrade(")
	if putIdx < 0 || dispIdx < 0 || putIdx > dispIdx {
		t.Error("ccSecrets.put must run before dispatchUpgrade to avoid a fetch-before-store race")
	}
	// secret 绝不写进 metadata。检查 taskMeta marshal 块不包含 GatewayURL/APIKey。
	taskMetaRe := regexp.MustCompile(`taskMeta, _ := json\.Marshal\(map\[string\]interface\{\}\{[^}]+\}`)
	taskMetaBlocks := taskMetaRe.FindAllString(body, -1)
	for _, block := range taskMetaBlocks {
		if strings.Contains(block, "GatewayURL") || strings.Contains(block, "APIKey") {
			t.Error("install secret must NOT be marshalled into task metadata")
			break
		}
	}

	// put 发生在 tx.Commit() 之前,所以 commit 失败会留下 orphan secret;
	// 必须在 commit 失败分支 evict 掉(TTL 兜底之外的即时清理)。锁住这条。
	commitIdx := strings.Index(body, "tx.Commit()")
	if putIdx < 0 || commitIdx < 0 || putIdx > commitIdx {
		t.Error("ccSecrets.put must run before tx.Commit so the daemon's immediate fetch can't miss the secret")
	}
	if !regexp.MustCompile(`if err := tx\.Commit\(\); err != nil \{[\s\S]*?ccSecrets\.evict\(`).MatchString(body) {
		t.Error("commit-failure branch must evict the orphan cc-octo secret")
	}
}

// install 复用 upgrade 的版本比对:空 fromVersion 必须被判为"比 latest 旧"才能放行
// install(否则版本比对会把首装拦成 Conflict)。锁住这条依赖。
func TestIsVersionOlder_EmptyIsOlderThanLatest(t *testing.T) {
	if !isVersionOlder("", "0.7.0") {
		t.Fatal(`isVersionOlder("", "0.7.0") 应为 true —— 否则未安装(空版本)的 install 会被版本比对拦成 Conflict`)
	}
}
