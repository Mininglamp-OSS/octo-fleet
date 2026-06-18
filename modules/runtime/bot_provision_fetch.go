package runtime

import (
	"strconv"

	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope" // swag @Success/@Failure type resolution
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// 决策三 A3 secret fetch endpoint.
//
// 老 heartbeat flow: claimPendingBotProvision 一步走原子 claim + 返 full
// payload. 在 SSE 模式下不能把 bot identifying info / claim_token 进 SSE
// stream (X-Accel buffer / event_log JSON / 中间代理 log 都可能泄), 所以
// SSE event 只 wake-up (推 command_id), daemon 收到后另起单次 HTTPS GET
// 这个 endpoint 拿完整 payload.
//
// 注意 (MI7 doc clarification): 这个 endpoint 返回 bot_uid + workspace_id +
// claim_token 但**不含 bot_token**. fleet bot 表不存 token — daemon 收到
// 后若 cmd.BotToken == "" 会另起 GET /v1/bot/:bot_uid/token 问 octo-server
// 拿. 所以 A3 secret 不进 SSE stream 是因为 fleet 本来就没 secret, 这里
// fetch 拿的是 bot identifying info + claim_token, 不是 secret 本身.
//
// 这个 endpoint 是"按 bot.id 的 claimPendingBotProvision":
//   - bot 必须 status=bot_minted: 走 atomic 转 dispatched + mint claim_token
//   - bot 已 status=dispatched: 幂等返回同 payload (daemon 可能 lost local
//     state 重新 fetch, claim_token 已 minted, 仍可用)
//   - 其它 status: 410 Gone (已 active / archived / draft / failed)
//
// ownership gate (A7 同 pattern): 必须 owner_uid + space_id 匹配 caller,
// 否则 daemon A 持自己 api_key 传别人的 bot.id 就能拿别人的 payload.

// botProvisionFetchResponse 是 fetch endpoint 显式 whitelist response 字段.
// F-4 (lml2468 review): 不复用 buildPendingBotProvision (含 bot_token 字段,
// 今天 m.BotToken="" 不漏 — 但未来若给 bot 表加 token cache 列, buildPending
// 会自动 include, SSE fetch endpoint 静默 leak secret. 改成显式 struct
// **永远不含 bot_token 字段**, future schema 变化不影响.
type botProvisionFetchResponse struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	RuntimeKind string `json:"runtime_kind"`
	WorkspaceID string `json:"workspace_id"`
	DisplayName string `json:"display_name"`
	BotUID      string `json:"bot_uid"`
	ClaimToken  string `json:"claim_token"`
}

// toFetchResponse 显式列字段, 防 future bot 表加列时 buildPendingBotProvision
// 自动 leak. 不包 bot_token. daemon 收到后若 cmd.BotToken=="" 自己问
// octo-server 拿 (exec_openclaw.go handleBotProvision).
func toFetchResponse(m *botModel) botProvisionFetchResponse {
	return botProvisionFetchResponse{
		ID:          m.Id,
		Action:      "bot.provision",
		RuntimeKind: m.RuntimeKind,
		WorkspaceID: m.WorkspaceID,
		DisplayName: m.Name,
		BotUID:      m.BotUID,
		ClaimToken:  m.ClaimToken,
	}
}

// GET /v1/bots/{bot_id}/provision?runtime_id=N
//
// :bot_id（== 原 command_id == bot.id，与 heartbeat buildPendingBotProvision
// 的 `id` 字段一致，daemon 端 handleBotProvision 已知道这个 shape）。
//
// runtime 绑定 (Jerry-Xin fleet#44 blocking): api_key 只绑 (owner, space),
// fleet 单凭 key 区分不出 caller 是哪台 daemon/runtime. 仅校验 owner+space
// 不够 —— 同 owner+space 下 daemon A 可以拿别的 runtime 的 bot.id 来 fetch,
// 把本该派给 daemon B 的 bot claim + ack 走, 绕过 heartbeat 路径
// (claimPendingBotProvision) 用 daemon_id 保证的路由. 所以这里要求 daemon
// 自报 runtime_id (订阅 SSE 时本就带了), 验它归 caller 所有 (同 sseEvents
// 的 A7 gate), 且 bot.runtime_id 必须等于它. claim UPDATE 也带 runtime_id
// 约束, 跟 claimPendingBotProvision 对齐.
//
// fetchBotProvision godoc
// @Summary      Fetch bot provision payload
// @Description  Daemon fetches the full bot.provision payload (workspace_id, bot_uid, claim_token; never bot_token) and atomically claims it (bot_minted->dispatched). Idempotent while dispatched; 409 once active/archived.
// @Tags         bot
// @ID           bot.provision.get
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        bot_id path int true "Bot ID (== provision command id)"
// @Param        runtime_id query int true "Caller's runtime ID (ownership-bound)"
// @Success      200 {object} envelope.Data[botProvisionFetchResponse] "provision payload"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      409 {object} envelope.Error "CONFLICT: not provisionable"
// @Router       /bots/{bot_id}/provision [get]
func (rt *Runtime) fetchBotProvision(c *wkhttp.Context) {
	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	idStr := c.Param("bot_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		responseError(c, errcode.Validation)
		return
	}

	runtimeIDStr := c.Query("runtime_id")
	runtimeID, err := strconv.ParseInt(runtimeIDStr, 10, 64)
	if err != nil || runtimeID <= 0 {
		responseError(c, errcode.Validation)
		return
	}

	// A7 ownership gate (同 sseEvents): runtime_id 必须属于 caller 的
	// (owner_uid, space_id), 否则 daemon A 持自己 api_key 传别人的
	// runtime_id 就绕过了路由绑定.
	own, err := rt.db.queryByID(runtimeID)
	if err != nil {
		rt.Error("fetchBotProvision: query runtime by id", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		responseError(c, errcode.InternalError)
		return
	}
	if own == nil || own.OwnerUID != ownerUID || own.SpaceID != spaceID {
		responseError(c, errcode.Forbidden)
		return
	}

	row, err := rt.db.queryBotByID(id)
	if err != nil {
		rt.Error("fetchBotProvision: queryBotByID", zap.Error(err), zap.Int64("id", id))
		responseError(c, errcode.InternalError)
		return
	}
	// ownership + routing gate: 不分 not-found vs forbidden, 防 enumeration.
	// row.RuntimeID != runtimeID 即"这个 bot 不归来取的这台 daemon" — 拒绝.
	if row == nil || row.OwnerUID != ownerUID || row.SpaceID != spaceID || row.RuntimeID != runtimeID {
		responseError(c, errcode.Forbidden)
		return
	}

	rt.respondBotProvisionByStatus(c, id, runtimeID, row)
}

// respondBotProvisionByStatus 按 row 当前 status 返响应. 在两个地方调:
// 1) fetchBotProvision 顶层 (初次进 endpoint)
// 2) race fallback (UPDATE WHERE status='bot_minted' affected=0, re-query 后)
//
// M3 fix (caster review final from codex): race fallback 必须真按新 status
// 重判, 不能直接 buildPendingBotProvision 给 active/archived 的 stale payload.
//   - dispatched: 返 payload (claim_token 已 minted, daemon 继续 ack 流程)
//   - active/archived/draft/failed: 返 410, 跟默认分支语义一致, daemon 端
//     dedup mark 然后跳, 不再尝试 mint
//   - bot_minted: 极少 (UPDATE affected=0 后又有人改回 bot_minted? 几乎不可能,
//     仍按 default 返 410 防死循环)
//
// F-4 (lml2468 review): 改用 toFetchResponse 不复用 buildPendingBotProvision.
func (rt *Runtime) respondBotProvisionByStatus(c *wkhttp.Context, id, runtimeID int64, row *botModel) {
	switch row.Status {
	case botStatusBotMinted:
		// 原子 claim + mint claim_token (同 claimPendingBotProvision 但
		// by id 而非 daemon_id pick-one). 双跑期: heartbeat
		// claimPendingBotProvision 仍可能并发尝试同一 row, UPDATE WHERE
		// status='bot_minted' 是 atomic, 同 row 只有一方成功.
		// runtime_id 约束 (fleet#44): claim 只在 bot 归这台 runtime 时成立,
		// 跟 claimPendingBotProvision 的 daemon_id 路由绑定对齐.
		token := randomToken()
		result, uerr := rt.db.session.UpdateBySql(
			`UPDATE bot SET status=?, claim_token=? WHERE id=? AND status=? AND runtime_id=?`,
			botStatusDispatched, token, id, botStatusBotMinted, runtimeID,
		).Exec()
		if uerr != nil {
			rt.Error("fetchBotProvision: claim update", zap.Error(uerr), zap.Int64("id", id))
			responseError(c, errcode.InternalError)
			return
		}
		affected, _ := result.RowsAffected()
		if affected == 0 {
			// race with heartbeat claim (or another concurrent fetch) —
			// re-query 拿当前 status, 真递归一次决定怎么返.
			newRow, qerr := rt.db.queryBotByID(id)
			if qerr != nil || newRow == nil {
				rt.Error("fetchBotProvision: re-query after race", zap.Error(qerr), zap.Int64("id", id))
				responseError(c, errcode.InternalError)
				return
			}
			// 防无限递归: 如果 re-query 后仍是 bot_minted (理论不可能因为我们刚
			// 才 UPDATE failed 意味着别人改了状态), 仍按 default 返 410.
			if newRow.Status == botStatusBotMinted {
				responseError(c, errcode.Conflict)
				return
			}
			rt.respondBotProvisionByStatus(c, id, runtimeID, newRow)
			return
		}
		row.Status = botStatusDispatched
		row.ClaimToken = token
		ResponseData(c, toFetchResponse(row))

	case botStatusDispatched:
		// 幂等 — daemon 重 fetch (本地状态丢 / SSE replay 重复推).
		// claim_token 已存 row, 同 payload 返回, daemon 继续 ack 流程.
		ResponseData(c, toFetchResponse(row))

	default:
		// active / archived / draft / failed — 不应该 fetch 到. 原本用 410
		// Gone 让 daemon 端 dedup 丢弃; 12-enum 无 410, 用 Conflict (非可
		// provision 状态) 最贴近, daemon 仍按非 2xx dedup-drop.
		responseError(c, errcode.Conflict)
	}
}
