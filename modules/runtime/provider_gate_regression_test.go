package runtime

import (
	"strings"
	"testing"
)

// 防未来 refactor 把 active-gate 摘掉:register 落库前必须过 IsActiveKind。
func TestRegister_HasActiveProviderGate(t *testing.T) {
	src := mustReadSource(t, "api.go")
	body := extractFuncBody(t, src, "register")
	if !strings.Contains(body, "IsActiveKind") {
		t.Errorf("register() must gate on rt.providers.IsActiveKind to drop disabled providers")
	}
}

// createBot 必须用 active gate 校验 runtime_kind。
func TestCreateBot_HasActiveProviderGate(t *testing.T) {
	src := mustReadSource(t, "bot.go")
	body := extractFuncBody(t, src, "createBot")
	if !strings.Contains(body, "IsActiveKind") {
		t.Errorf("createBot() must validate runtime_kind via rt.providers.IsActiveKind")
	}
	if strings.Contains(body, "isValidRuntimeKind") {
		t.Errorf("createBot() must NOT use the removed isValidRuntimeKind")
	}
}

// upgradeInit 必须按 active provider 把关。
func TestUpgradeInit_HasActiveProviderGate(t *testing.T) {
	src := mustReadSource(t, "upgrade.go")
	body := extractFuncBody(t, src, "upgradeInit")
	if !strings.Contains(body, "IsActiveKind") {
		t.Errorf("upgradeInit() must gate component on rt.providers.IsActiveKind")
	}
	if strings.Contains(body, "isProviderComponent") {
		t.Errorf("upgradeInit() must NOT use the removed isProviderComponent")
	}
}

// list / listBots 必须按 active provider 过滤(soft hide disabled)。
func TestListSQL_FiltersDisabledProviders(t *testing.T) {
	dbSrc := mustReadSource(t, "db.go")
	if !strings.Contains(extractFuncBody(t, dbSrc, "listBySpaceIDAndOwner"), "provider IN") {
		t.Errorf("listBySpaceIDAndOwner must filter `provider IN (active...)`")
	}
	botSrc := mustReadSource(t, "bot.go")
	if !strings.Contains(extractFuncBody(t, botSrc, "listBotsBySpace"), "runtime_kind IN") {
		t.Errorf("listBotsBySpace must filter `runtime_kind IN (active...)`")
	}
}
