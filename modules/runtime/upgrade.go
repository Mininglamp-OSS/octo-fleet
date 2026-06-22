package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-fleet/internal/auth"
	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope" // swag @Success/@Failure type resolution
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

const (
	componentDaemon = "octo-daemon"
	componentPlugin = "octo"

	daemonUpgradeTimeoutSec = 120 // 2 分钟
	pluginUpgradeTimeoutSec = 600 // 10 分钟（npm install + 依赖 + gateway restart）

	// fallbackComponentTimeoutSec 是 sweeper 对非 daemon/plugin/active component
	// (disabled provider 残留 / 未知)的统一兜底 timeout。30min = 最大已知
	// provider timeout(hermes 20min)的 1.5×,足够保守,确保残留 task 终会 timeout
	// 而不会永占该 daemon 的单 in-progress 名额。
	fallbackComponentTimeoutSec = 1800
)

// upgradeInit godoc
// @Summary      Create an upgrade task
// @Description  Queue a daemon / runtime-component / plugin upgrade for a daemon the caller owns. Rejected if already up to date or one is already in progress.
// @Tags         upgrade
// @ID           upgrade.create
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        body body upgradeInitReq true "Upgrade request (daemon_id, space_id, component, runtime_id)"
// @Success      201 {object} envelope.Data[upgradeInitResp] "task created"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      409 {object} envelope.Error "CONFLICT"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /upgrades [post]
func (rt *Runtime) upgradeInit(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()

	var req upgradeInitReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}
	if req.DaemonID == "" || req.SpaceID == "" {
		responseError(c, errcode.Validation)
		return
	}
	component := req.Component
	if component == "" {
		component = componentDaemon
	}

	// 1. 校验 space — FLEET MIGRATION: trust JWT.space_id.
	if !auth.MatchesSpace(c, req.SpaceID) {
		responseError(c, errcode.Forbidden)
		return
	}
	_ = loginUID

	// 2. 查 daemon
	var daemon agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("space_id=? AND daemon_id=? AND owner_uid=?", req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&daemon)
	if err != nil || daemon.DaemonID == "" {
		responseError(c, errcode.Forbidden)
		return
	}

	// 3. 检查 daemon 至少一个 runtime 在线
	var onlineCount int
	rt.db.session.SelectBySql(
		"SELECT COUNT(*) FROM agent_runtime WHERE space_id=? AND daemon_id=? AND owner_uid=? AND status='online'",
		req.SpaceID, req.DaemonID, loginUID,
	).LoadOne(&onlineCount)
	if onlineCount == 0 {
		responseError(c, errcode.Conflict)
		return
	}

	// 按 component 分流校验 + 收集任务字段
	switch {
	case component == componentDaemon:
		rt.createDaemonUpgradeTask(c, loginUID, &req, &daemon)
	case isPluginComponent(component):
		rt.createPluginUpgradeTask(c, loginUID, &req, &daemon, component)
	case rt.providers.IsActiveKind(component):
		rt.createComponentUpgradeTask(c, loginUID, &req, &daemon, component)
	default:
		responseError(c, errcode.Validation)
	}
}

// createComponentUpgradeTask 处理 active provider 组件（claude/openclaw）升级。
// 校验：runtime_id 归属当前用户、runtime.Provider == component、当前版本严格落后于 latest。
func (rt *Runtime) createComponentUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel, component string) {
	if req.RuntimeID == 0 {
		responseError(c, errcode.Validation)
		return
	}

	// 查 runtime 并强校验：provider 必须等于 component
	var runtime agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("id=? AND space_id=? AND daemon_id=? AND owner_uid=?",
			req.RuntimeID, req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&runtime)
	if err != nil || runtime.Id == 0 {
		responseError(c, errcode.Forbidden)
		return
	}
	if runtime.Provider != component {
		responseError(c, errcode.Validation)
		return
	}

	// 版本对比：runtime.Version 是 daemon 上报的当前版本
	fromVersion := runtime.Version
	if fromVersion == "" {
		responseError(c, errcode.Validation)
		return
	}

	var versionRow struct {
		LatestVersion string `db:"latest_version"`
	}
	_, err = rt.db.session.SelectBySql(
		"SELECT latest_version FROM runtime_latest_version WHERE component=?",
		component,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		responseError(c, errcode.Validation)
		return
	}

	// 严格落后检查：dev/unknown 视为比任何正式版本都旧
	if fromVersion == versionRow.LatestVersion {
		responseError(c, errcode.Conflict)
		return
	}
	if fromVersion != "dev" && fromVersion != "unknown" {
		if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
			responseError(c, errcode.Conflict)
			return
		}
	}

	// runtime_id 放到 task.metadata，供 completeUpgradeIfMatchedWithRuntime 关单时对齐
	taskMeta, _ := json.Marshal(map[string]interface{}{
		"runtime_id": req.RuntimeID,
	})

	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   component,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: "",
		Checksum:    "",
		Metadata:    string(taskMeta),
		RuntimeID:   req.RuntimeID,
	})
}

// octo-daemon 升级：现有逻辑
func (rt *Runtime) createDaemonUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel) {
	// OS 检查
	var deviceInfo struct {
		OS   string `json:"os"`
		Arch string `json:"arch"`
	}
	json.Unmarshal([]byte(daemon.DeviceInfo), &deviceInfo)
	if deviceInfo.OS == "windows" {
		responseError(c, errcode.Validation)
		return
	}

	// 查最新版本 + release_meta
	var versionRow struct {
		LatestVersion string `db:"latest_version"`
		ReleaseMeta   string `db:"release_meta"`
	}
	_, err := rt.db.session.SelectBySql(
		"SELECT latest_version, COALESCE(release_meta,'') as release_meta FROM runtime_latest_version WHERE component=?",
		componentDaemon,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		responseError(c, errcode.Validation)
		return
	}

	// 当前版本
	var metaJSON struct {
		CLIVersion string `json:"cli_version"`
	}
	json.Unmarshal([]byte(daemon.Metadata), &metaJSON)
	fromVersion := metaJSON.CLIVersion

	if fromVersion != "" && fromVersion == versionRow.LatestVersion {
		responseError(c, errcode.Conflict)
		return
	}
	if fromVersion != "" && fromVersion != "dev" && fromVersion != "unknown" {
		if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
			responseError(c, errcode.Conflict)
			return
		}
	}

	// 匹配 asset
	if versionRow.ReleaseMeta == "" {
		responseError(c, errcode.Validation)
		return
	}
	var meta releaseMetaJSON
	if err := json.Unmarshal([]byte(versionRow.ReleaseMeta), &meta); err != nil {
		rt.Error("parse release_meta", zap.Error(err))
		// corrupt server-stored release_meta, not client input → 500
		responseError(c, errcode.InternalError)
		return
	}
	osName := normalizeOS(deviceInfo.OS)
	archName := normalizeArch(deviceInfo.Arch)
	var matchedAsset *releaseAssetJSON
	for i, a := range meta.Assets {
		if a.Kind == "archive" && a.OS == osName && a.Arch == archName {
			matchedAsset = &meta.Assets[i]
			break
		}
	}
	if matchedAsset == nil {
		responseError(c, errcode.Validation)
		return
	}
	checksum := meta.Checksums[matchedAsset.Name]
	if checksum == "" {
		responseError(c, errcode.Validation)
		return
	}

	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   componentDaemon,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: matchedAsset.URL,
		Checksum:    checksum,
		Metadata:    "",
	})
}

// octo 插件升级
func (rt *Runtime) createPluginUpgradeTask(c *wkhttp.Context, loginUID string, req *upgradeInitReq, daemon *agentRuntimeModel, component string) {
	if req.RuntimeID == 0 {
		responseError(c, errcode.Validation)
		return
	}

	// 查 runtime（归属 loginUID + 同 daemon_id）
	var runtime agentRuntimeModel
	_, err := rt.db.session.Select("*").From("agent_runtime").
		Where("id=? AND space_id=? AND daemon_id=? AND owner_uid=?",
			req.RuntimeID, req.SpaceID, req.DaemonID, loginUID).
		Limit(1).Load(&runtime)
	if err != nil || runtime.Id == 0 {
		responseError(c, errcode.Forbidden)
		return
	}
	// 组件必须是该 provider 的 octo 适配插件（octo↔openclaw / cc-octo↔claude）
	if !validPluginForProvider(component, runtime.Provider) {
		responseError(c, errcode.Validation)
		return
	}

	// 从 metadata.plugins 里找当前插件版本
	var metaJSON struct {
		Plugins []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"plugins"`
	}
	json.Unmarshal([]byte(runtime.Metadata), &metaJSON)
	fromVersion := ""
	for _, p := range metaJSON.Plugins {
		if p.Name == component {
			fromVersion = p.Version
			break
		}
	}
	// fromVersion == "" 表示该插件尚未安装(install)。
	//   - openclaw 的 octo 适配插件(componentPlugin): 一键 install 到 latest,无需额外配置。
	//   - cc-octo: 一键 install 需用户提供 LLM 网关 url + key(随请求带 gateway_url/api_key);
	//     缺任一则拒绝。secret 不入库,受理后进内存 transient store 中转给 daemon。
	isInstall := fromVersion == ""
	if isInstall {
		switch component {
		case componentPlugin:
			// ok, no secret needed
		case componentCcOcto:
			if req.GatewayURL == "" || req.APIKey == "" {
				responseError(c, errcode.Validation)
				return
			}
			if !isAllowedGatewayURL(req.GatewayURL) {
				responseError(c, errcode.Validation)
				return
			}
		default:
			responseError(c, errcode.Validation)
			return
		}
	}

	// 查最新版本
	var versionRow struct {
		LatestVersion string `db:"latest_version"`
	}
	_, err = rt.db.session.SelectBySql(
		"SELECT latest_version FROM runtime_latest_version WHERE component=?",
		component,
	).Load(&versionRow)
	if err != nil || versionRow.LatestVersion == "" {
		responseError(c, errcode.Validation)
		return
	}

	// 版本校验：必须严格落后
	if fromVersion == versionRow.LatestVersion {
		responseError(c, errcode.Conflict)
		return
	}
	if !isVersionOlder(fromVersion, versionRow.LatestVersion) {
		responseError(c, errcode.Conflict)
		return
	}

	// 任务 metadata 记录 runtime_id（用于关单校验）
	taskMeta, _ := json.Marshal(map[string]interface{}{
		"runtime_id": req.RuntimeID,
	})

	var ccSecret *ccOctoSecret
	if component == componentCcOcto && isInstall {
		ccSecret = &ccOctoSecret{GatewayURL: req.GatewayURL, APIKey: req.APIKey}
	}
	rt.insertUpgradeTask(c, insertTaskArgs{
		SpaceID:     req.SpaceID,
		DaemonID:    req.DaemonID,
		OwnerUID:    loginUID,
		Component:   component,
		FromVersion: fromVersion,
		ToVersion:   versionRow.LatestVersion,
		DownloadURL: "",
		Checksum:    "",
		Metadata:    string(taskMeta),
		RuntimeID:   req.RuntimeID,
		CcSecret:    ccSecret,
	})
}

type insertTaskArgs struct {
	SpaceID     string
	DaemonID    string
	OwnerUID    string
	Component   string
	FromVersion string
	ToVersion   string
	DownloadURL string
	Checksum    string
	Metadata    string
	// RuntimeID: 决策三 SSE 用 — component/plugin upgrade 填具体 runtime,
	// daemon 自身 upgrade 留 0 (dispatcher 走 firstRuntimeIDForDaemon
	// fallback). 仅用于 SSE push target, 不写 runtime_upgrade_task 表.
	RuntimeID int64
	// CcSecret: cc-octo 一键安装的 LLM 网关+key。非 nil 时,insertUpgradeTask 在
	// SSE dispatch 之前存进内存 transient store(绝不入库/不进 metadata)。
	CcSecret *ccOctoSecret
}

// 互斥：同 daemon_id 只允许一个 in-progress 任务（无论 component）
// 关键：没有现有任务时 SELECT COUNT(*) ... FOR UPDATE 不锁任何行，并发 upgradeInit
// 仍可能都看到 0 各自插入。先锁 agent_runtime 里 daemon_id 对应的某一行强制串行化，
// 所有同 daemon 的并发请求都会卡在同一把锁上。
func (rt *Runtime) insertUpgradeTask(c *wkhttp.Context, args insertTaskArgs) {
	tx, err := rt.db.session.Begin()
	if err != nil {
		responseError(c, errcode.InternalError)
		return
	}
	defer tx.RollbackUnlessCommitted()

	// 先锁 agent_runtime 中该 (daemon, space, owner) 的任一行，强制同 owner 同 daemon
	// 并发串行。
	//
	// v3.3.1 §C.3 (Jerry-Xin Critical 3, three-round): added owner_uid to
	// both the FOR UPDATE row lock and the active-count check. Before
	// this, the lock was per (daemon, space) — two distinct owners sharing
	// a daemon_id (legal after runtime-20260606-01) would serialize on
	// the same row and the active-count would include each other's
	// in-progress upgrades, blocking unrelated tenants and effectively
	// causing cross-tenant DoS. Scoping by owner gives each tenant their
	// own concurrency budget.
	var lockRow struct {
		ID int64 `db:"id"`
	}
	_, err = tx.SelectBySql(
		`SELECT id FROM agent_runtime WHERE daemon_id=? AND space_id=? AND owner_uid=? ORDER BY id LIMIT 1 FOR UPDATE`,
		args.DaemonID, args.SpaceID, args.OwnerUID,
	).Load(&lockRow)
	if err != nil {
		responseError(c, errcode.InternalError)
		return
	}

	var activeCount int
	// F-1 (lml2468 review): LoadOne err 必须接 — swallow 会让 activeCount=0
	// 绕过 "已 in-progress" 检查, 重复 INSERT pending row, 破坏单 in-progress
	// per-daemon 不变量.
	if err := tx.SelectBySql(
		`SELECT COUNT(*) FROM runtime_upgrade_task
		 WHERE daemon_id=? AND space_id=? AND owner_uid=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')`,
		args.DaemonID, args.SpaceID, args.OwnerUID,
	).LoadOne(&activeCount); err != nil {
		rt.Error("active upgrade count query", zap.Error(err), zap.String("daemon_id", args.DaemonID))
		responseError(c, errcode.InternalError)
		return
	}
	if activeCount > 0 {
		responseError(c, errcode.Conflict)
		return
	}

	taskID := fmt.Sprintf("upgrade_%d", snowflakeID())
	_, err = tx.InsertBySql(
		`INSERT INTO runtime_upgrade_task (id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, metadata, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		taskID, args.SpaceID, args.DaemonID, args.OwnerUID, args.Component,
		args.FromVersion, args.ToVersion, args.DownloadURL, args.Checksum, args.Metadata,
	).Exec()
	if err != nil {
		rt.Error("create upgrade task", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	// F-1 (lml2468 review): tx.Commit() err 必须接 — 之前 swallow 会返
	// 200 OK 但 taskID 不存在, SSE dispatch 推 event daemon 追不到行,
	// 静默腐败. 接 err → 500 让 daemon 不收到 event, web 看到 5xx 重试.
	if err := tx.Commit(); err != nil {
		rt.Error("commit upgrade task", zap.Error(err), zap.String("task_id", taskID))
		responseError(c, errcode.InternalError)
		return
	}

	// cc-octo install secret 必须在 dispatch 之前入 store —— dispatchUpgrade 会
	// 唤醒 daemon 立刻来 fetch,晚于此即 404 竞态。secret 不持久化,只内存中转。
	if args.CcSecret != nil {
		rt.ccSecrets.put(taskID, *args.CcSecret)
	}

	// 决策三 SSE 反向派发 (Phase A 双跑): component/plugin upgrade 已知
	// runtime_id 直接推; daemon 自身 upgrade 走 firstRuntimeIDForDaemon
	// fallback (daemon 进程的任一 runtime SSE 收到都触发 upgrade handler).
	// heartbeat claimPendingXxx 仍兜底.
	targetRuntimeID := args.RuntimeID
	if targetRuntimeID <= 0 {
		if rid, rerr := rt.db.firstRuntimeIDForDaemon(args.SpaceID, args.DaemonID, args.OwnerUID); rerr == nil && rid > 0 {
			targetRuntimeID = rid
		} else if rerr != nil {
			rt.Warn("sse: firstRuntimeIDForDaemon (upgrade)", zap.Error(rerr), zap.String("daemon_id", args.DaemonID))
		}
	}
	if targetRuntimeID > 0 {
		rt.dispatchUpgrade(targetRuntimeID, args.SpaceID, args.OwnerUID, &upgradeTask{
			ID:          taskID,
			SpaceID:     args.SpaceID,
			DaemonID:    args.DaemonID,
			OwnerUID:    args.OwnerUID,
			Component:   args.Component,
			FromVersion: args.FromVersion,
			ToVersion:   args.ToVersion,
			DownloadURL: args.DownloadURL,
			Checksum:    args.Checksum,
			Metadata:    args.Metadata,
			Status:      "pending",
		})
	}

	ResponseCreated(c, upgradeInitResp{TaskID: taskID})
}

// upgradeGet godoc
// @Summary      Get upgrade task status
// @Description  Read the current status of an upgrade task the caller owns.
// @Tags         upgrade
// @ID           upgrade.get
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        task_id path string true "Upgrade task ID"
// @Success      200 {object} envelope.Data[upgradeGetResp] "task status"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Router       /upgrades/{task_id} [get]
func (rt *Runtime) upgradeGet(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	taskID := c.Param("task_id")

	var task upgradeTask
	_, err := rt.db.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status, COALESCE(error_msg,'') as error_msg
		 FROM runtime_upgrade_task WHERE id=?`, taskID,
	).Load(&task)
	if err != nil || task.ID == "" {
		responseError(c, errcode.NotFound)
		return
	}

	if task.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}

	ResponseData(c, upgradeGetResp{
		ID:          task.ID,
		Component:   task.Component,
		Status:      task.Status,
		FromVersion: task.FromVersion,
		ToVersion:   task.ToVersion,
		ErrorMsg:    task.ErrorMsg,
	})
}

// upgradeReport godoc
// @Summary      Report upgrade progress
// @Description  Daemon reports an upgrade task's state transition (downloading / installing / succeeded / failed). Rejected on invalid transition.
// @Tags         upgrade
// @ID           upgrade.report
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        task_id path string true "Upgrade task ID"
// @Param        body body upgradeReportReq true "status + error"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "recorded"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      409 {object} envelope.Error "CONFLICT"
// @Router       /upgrades/{task_id}/report [post]
func (rt *Runtime) upgradeReport(c *wkhttp.Context) {
	ownerUID := c.MustGet("uid").(string)
	apiSpaceID := c.MustGet("space_id").(string)
	taskID := c.Param("task_id")

	var req upgradeReportReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}

	var task upgradeTask
	_, err := rt.db.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status, COALESCE(error_msg,'') as error_msg
		 FROM runtime_upgrade_task WHERE id=?`, taskID,
	).Load(&task)
	if err != nil || task.ID == "" {
		responseError(c, errcode.NotFound)
		return
	}
	if task.SpaceID != apiSpaceID || task.OwnerUID != ownerUID {
		responseError(c, errcode.Forbidden)
		return
	}

	// 按 component 放行状态流转
	allowed := validTransitionsFrom(task.Component, req.Status)
	if allowed == nil {
		responseError(c, errcode.Conflict)
		return
	}

	result, err := rt.db.session.UpdateBySql(
		fmt.Sprintf(
			`UPDATE runtime_upgrade_task SET status=?, error_msg=?, updated_at=NOW()
			 WHERE id=? AND status IN (%s)`,
			placeholders(len(allowed)),
		),
		append([]interface{}{req.Status, req.Error, taskID}, toInterfaces(allowed)...)...,
	).Exec()
	if err != nil {
		rt.Error("update upgrade task", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		responseError(c, errcode.Conflict)
		return
	}

	// cc-octo install secret 用完即清：failed 终态 report 后驱逐；
	// completed 关单由 register/close-out 路径处理（见 api.go 中 completeUpgradeIfMatchedWithRuntime 调用方），TTL 是兜底。
	if task.Component == componentCcOcto && req.Status == "failed" {
		rt.ccSecrets.evict(taskID)
	}
	ResponseEmpty(c)
}

// 按 component 返回目标状态允许从哪些前置状态流转而来；nil 表示不允许此状态。
// octo-daemon 有完整 5 态机；其他所有组件（plugin + provider 组件）
// 都走精简的 dispatched → installing → (completed by register / failed) 3 态机。
func validTransitionsFrom(component, target string) []string {
	// SSE fast-path 不经 claimPendingUpgrade (heartbeat 路径才把 row 转 dispatched),
	// 所以 daemon 通过 SSE 直接 report 第一跳时 row 仍是 'pending'. 第一跳
	// (downloading / installing) 接受 pending|dispatched 两种起始状态
	// (codex review final BLOCKER): 只接受 'dispatched' 时, SSE 路径首个
	// report 静默 affected=0, daemon 端 dedup 已 mark, 后续 heartbeat 又
	// block 不能 re-deliver → task 永远卡到 sweeper timeout.
	if component == componentDaemon {
		switch target {
		case "downloading":
			return []string{"pending", "dispatched"}
		case "installing":
			return []string{"downloading"}
		case "restarting":
			return []string{"installing"}
		case "failed":
			return []string{"pending", "dispatched", "downloading", "installing", "restarting"}
		}
		return nil
	}
	// plugin + provider 组件
	switch target {
	case "installing":
		return []string{"pending", "dispatched"}
	case "failed":
		return []string{"pending", "dispatched", "installing"}
	}
	return nil
}

// DB operations for upgrade

// claim：去掉 component 过滤，同 daemon_id 下取最新 pending task
func (d *runtimeDB) claimPendingUpgrade(spaceID, daemonID, ownerUID string) (*upgradeTask, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var task upgradeTask
	// owner_uid filter (reviewer fleet#24 Jerry-Xin): runtime_upgrade_task
	// already carries owner_uid; the previous query relied on
	// (space_id, daemon_id) alone which is vulnerable to same-space
	// cross-owner daemon_id collisions (register allows caller-supplied
	// daemon_id without uniqueness).
	_, err = tx.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status
		 FROM runtime_upgrade_task
		 WHERE space_id=? AND daemon_id=? AND owner_uid=? AND status='pending'
		 ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
		spaceID, daemonID, ownerUID,
	).Load(&task)
	if err != nil {
		return nil, err
	}
	if task.ID == "" {
		return nil, nil
	}

	_, err = tx.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='dispatched', updated_at=NOW() WHERE id=?`,
		task.ID,
	).Exec()
	if err != nil {
		return nil, err
	}
	tx.Commit()

	task.Status = "dispatched"
	return &task, nil
}

// daemon 升级关单：按 (space, daemon_id, owner, component, version) 五元组.
//
// v3.3.1 §C.2 (Jerry-Xin Critical 2, three-round): added spaceID + ownerUID
// to the WHERE clause. Without these, the runtime-20260606-01 schema
// migration's per-owner daemon rows could be "completed" by another
// owner's register call sharing the same daemon_id + target version —
// user B's daemon registration would silently mark user A's in-progress
// upgrade complete. The fix scopes the UPDATE to the same (space, owner)
// boundary as the task that created it.
func (d *runtimeDB) completeUpgradeIfMatched(daemonID, spaceID, ownerUID, component, version string) {
	// F-3 (lml2468 review): 加 'pending' 对称 R3 first-hop 放宽. SSE
	// fast-path 不经 claimPendingUpgrade, row 可能仍 pending 时 daemon
	// 完成 upgrade → register 触发 close-out. 不接 pending 会让 task 卡
	// 等 sweeper timeout.
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='completed', updated_at=NOW()
		 WHERE daemon_id=? AND space_id=? AND owner_uid=? AND component=? AND to_version=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')`,
		daemonID, spaceID, ownerUID, component, version,
	).Exec()
}

// 插件升级关单：候选集按 (space, daemon, owner, component, in-progress, runtime_id)
// 过滤；版本对比不要求精确相等（npx 安装的版本可能比任务创建时的 to_version 更新），
// 只要 actual_version >= to_version 就关单。
//
// v3.3.1 §C.2: same (space, owner) scoping as completeUpgradeIfMatched —
// the candidates SELECT is the cross-owner leak vector under the
// post-§4.4 schema, not just the final UPDATE.
//
// 返回被关单的任务 ID 列表，供调用方做后续处理（如驱逐 cc-octo install secret）。
func (d *runtimeDB) completeUpgradeIfMatchedWithRuntime(daemonID, spaceID, ownerUID, component, actualVersion string, runtimeID int64) []string {
	var candidates []upgradeTask
	var completedIDs []string
	// F-3 (lml2468 review): 加 'pending' 对称 R3, 跟 completeUpgradeIfMatched 同理由.
	_, err := d.session.SelectBySql(
		`SELECT id, space_id, daemon_id, owner_uid, component, from_version, to_version, download_url, checksum, COALESCE(metadata,'') as metadata, status
		 FROM runtime_upgrade_task
		 WHERE daemon_id=? AND space_id=? AND owner_uid=? AND component=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')`,
		daemonID, spaceID, ownerUID, component,
	).Load(&candidates)
	if err != nil {
		return completedIDs
	}
	for _, t := range candidates {
		// runtime_id 归属校验
		var m struct {
			RuntimeID int64 `json:"runtime_id"`
		}
		if t.Metadata != "" {
			json.Unmarshal([]byte(t.Metadata), &m)
		}
		if m.RuntimeID != runtimeID {
			continue
		}
		// 版本校验：actual >= to_version
		if actualVersion != t.ToVersion && !isVersionOlder(t.ToVersion, actualVersion) {
			continue
		}
		_, err := d.session.UpdateBySql(
			`UPDATE runtime_upgrade_task SET status='completed', updated_at=NOW() WHERE id=?`,
			t.ID,
		).Exec()
		if err == nil {
			completedIDs = append(completedIDs, t.ID)
		}
	}
	return completedIDs
}

// sweeper：按 component 差异化 timeout。activeProviders 来自 provider registry。
// 兜底：任何非 daemon/plugin/active 的 in-progress task(如残留 disabled provider)
// 也必须被统一 timeout，否则它会永占该 daemon 的单 in-progress 名额、锁死后续升级。
func (d *runtimeDB) timeoutStaleUpgrades(activeProviders map[string]int) {
	// octo-daemon: 120s，完整 5 态机
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
		 WHERE component=?
		 AND status IN ('pending','dispatched','downloading','installing','restarting')
		 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
		componentDaemon, daemonUpgradeTimeoutSec,
	).Exec()
	// plugin: 600s，精简 3 态机(octo + cc-octo 同属插件桶)
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
		 WHERE component IN (?, ?)
		 AND status IN ('pending','dispatched','installing')
		 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
		componentPlugin, componentCcOcto, pluginUpgradeTimeoutSec,
	).Exec()
	// active provider 组件：各自 timeout，3 态机
	known := []interface{}{componentDaemon, componentPlugin, componentCcOcto}
	for component, timeoutSec := range activeProviders {
		known = append(known, component)
		d.session.UpdateBySql(
			`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out', updated_at=NOW()
			 WHERE component=?
			 AND status IN ('pending','dispatched','installing')
			 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
			component, timeoutSec,
		).Exec()
	}
	// 强兜底：其余所有 component(disabled provider 残留 / 未知)统一 1800s timeout。
	ph := placeholders(len(known)) // 复用本包 helper，勿用局部变量遮蔽
	args := append([]interface{}{}, known...)
	args = append(args, fallbackComponentTimeoutSec)
	d.session.UpdateBySql(
		`UPDATE runtime_upgrade_task SET status='timeout', error_msg='upgrade timed out (fallback)', updated_at=NOW()
		 WHERE component NOT IN (`+ph+`)
		 AND status IN ('pending','dispatched','downloading','installing','restarting')
		 AND updated_at < DATE_SUB(NOW(), INTERVAL ? SECOND)`,
		args...,
	).Exec()
}

// helpers

func normalizeOS(os string) string {
	switch strings.ToLower(os) {
	case "macos":
		return "darwin"
	default:
		return strings.ToLower(os)
	}
}

func normalizeArch(arch string) string {
	switch strings.ToLower(arch) {
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		return strings.ToLower(arch)
	}
}

func snowflakeID() int64 {
	return time.Now().UnixNano()
}

func placeholders(n int) string {
	p := make([]string, n)
	for i := range p {
		p[i] = "?"
	}
	return strings.Join(p, ",")
}

func toInterfaces(ss []string) []interface{} {
	r := make([]interface{}, len(ss))
	for i, s := range ss {
		r[i] = s
	}
	return r
}
