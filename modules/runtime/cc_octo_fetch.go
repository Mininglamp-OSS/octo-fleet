package runtime

import (
	"encoding/json"
	"strconv"

	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope"
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// ccOctoConfigResponse 是 daemon 取走的 cc-octo 安装 secret（LLM 网关 + key）。
// secret 只经此端点单独 fetch、永不进 SSE stream / event_log / DB —— 同
// bot.provision A3「wake-up + 独立 fetch」范式。
type ccOctoConfigResponse struct {
	GatewayURL string `json:"gateway_url"`
	APIKey     string `json:"api_key"`
}

// GET /v1/upgrades/{task_id}/cc-octo-config?runtime_id=N
//
// ownership gate（对齐 fetchBotProvision A7）：
//   - owner_uid + space_id 来自 daemon api_key
//   - runtime_id 自报，必须归 caller（queryByID），否则同 owner+space 下
//     daemon A 可拿别的 runtime 的 task secret
//   - runtime_id 必须等于 task.metadata 记录的 runtime_id
//   - task.status 必须 in-flight(pending/dispatched/installing);终态 → 409
//   - secret 命中 → 200 {url,key}
//   - secret 缺失:install task(from_version=="")→ 409(daemon report failed,
//     不可静默跑无 key 的普通 upgrade);普通 upgrade → 404(daemon 走普通 upgrade)。
func (rt *Runtime) fetchCcOctoConfig(c *wkhttp.Context) {
	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)
	taskID := c.Param("task_id")
	if taskID == "" {
		responseError(c, errcode.Validation)
		return
	}

	runtimeID, err := strconv.ParseInt(c.Query("runtime_id"), 10, 64)
	if err != nil || runtimeID <= 0 {
		responseError(c, errcode.Validation)
		return
	}

	// runtime ownership gate.
	own, err := rt.db.queryByID(runtimeID)
	if err != nil {
		rt.Error("fetchCcOctoConfig: queryByID", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		responseError(c, errcode.InternalError)
		return
	}
	if own == nil || own.OwnerUID != ownerUID || own.SpaceID != spaceID {
		responseError(c, errcode.Forbidden)
		return
	}

	// task ownership + runtime 绑定校验：task 必须归 caller、cc-octo、且其记录的
	// runtime_id 等于自报的 runtime_id。from_version 用来区分 install(空)与普通 upgrade。
	var task upgradeTask
	_, err = rt.db.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, COALESCE(metadata,'') as metadata, status
		 FROM runtime_upgrade_task WHERE id=?`, taskID,
	).Load(&task)
	if err != nil || task.ID == "" {
		responseError(c, errcode.NotFound)
		return
	}
	if task.OwnerUID != ownerUID || task.SpaceID != spaceID || task.Component != componentCcOcto {
		responseError(c, errcode.Forbidden)
		return
	}
	var meta struct {
		RuntimeID int64 `json:"runtime_id"`
	}
	if task.Metadata != "" {
		json.Unmarshal([]byte(task.Metadata), &meta)
	}
	if meta.RuntimeID != runtimeID {
		responseError(c, errcode.Forbidden)
		return
	}

	// 状态校验：只有 in-flight task 可 fetch。终态(completed/failed/timeout)→ 409,
	// daemon 不应再处理(且 secret 该已被 sweeper/TTL 回收)。
	switch task.Status {
	case "pending", "dispatched", "installing":
		// in-flight, ok
	default:
		responseError(c, errcode.Conflict)
		return
	}

	isInstall := task.FromVersion == ""
	secret, ok := rt.ccSecrets.get(taskID)
	if !ok {
		if isInstall {
			// install 但 secret 缺失/过期 → 不能让 daemon 静默跑「无 key 的普通
			// upgrade」装出没配置的 cc-octo。返 409,daemon 明确 report failed,
			// 用户重新发起安装(重填表单)。
			responseError(c, errcode.Conflict)
			return
		}
		// 普通 upgrade(已装,升新版):本就无 secret,返 404,daemon 走普通 upgrade 路径。
		responseError(c, errcode.NotFound)
		return
	}
	ResponseData(c, ccOctoConfigResponse{GatewayURL: secret.GatewayURL, APIKey: secret.APIKey})
}
