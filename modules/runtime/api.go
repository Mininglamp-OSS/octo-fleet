package runtime

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-fleet/internal/auth"
	// swag resolves envelope.Data[...] / envelope.Error in @Success/@Failure
	// annotations against this import (skill B章 prerequisite). Blank because
	// handlers emit envelopes via the resp.go helpers, not direct refs.
	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope"
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

type Runtime struct {
	ctx *config.Context
	log.Log
	db        runtimeDB
	eventDB   eventLogDB
	sseHub    *sseHub
	providers *providerRegistry
}

func New(ctx *config.Context) *Runtime {
	rt := &Runtime{
		ctx:     ctx,
		Log:     log.NewTLog("Runtime"),
		db:      *newRuntimeDB(ctx),
		eventDB: *newEventLogDB(ctx),
		sseHub:  newSseHub(),
	}
	rt.providers = newProviderRegistry(&rt.db)
	go rt.providers.refreshLoop()

	go rt.runSweeper()
	go rt.runEventLogSweeper()

	return rt
}

func (rt *Runtime) Route(r *wkhttp.WKHttp) {
	// FLEET MIGRATION: daemon endpoints authenticated by api_key Bearer
	// verified against octo-server's POST /v1/auth/verify-api-key
	// (with verifyCache TTL 60s). web endpoints authenticated by session
	// token verified against POST /v1/auth/verify?include=context.
	// The earlier JWT/JWKS scheme was removed in Phase 4 of decisions 1+2
	// (server PR #290 / fleet PR #24 / matter PR #78); fleet itself no
	// longer holds or verifies JWTs.
	// Gateway mounts fleet at <host>/fleet/api/ — the `fleet/api` service
	// segment is added by nginx and does NOT appear in the spec (A.1/R6).
	// Paths here are /v1/<resource>; daemon vs web callers are separated by
	// JWT scope (auth.Middleware), not by a path segment. The two groups
	// share the /v1 prefix; gin routes by method + sub-path.
	daemon := r.Group("/v1", auth.Middleware("daemon"))
	{
		daemon.POST("/runtimes", rt.register)                          // register/upsert this daemon's runtimes
		daemon.POST("/runtimes/:runtime_id/heartbeat", rt.heartbeat)   // liveness + pull pending commands
		daemon.POST("/runtimes/_deregister", rt.deregister)            // batch mark offline
		daemon.GET("/runtimes/:runtime_id/events", rt.sseEvents)       // per-runtime SSE reverse-dispatch stream
		daemon.GET("/bots/:bot_id/provision", rt.fetchBotProvision)    // fetch full bot.provision payload
		daemon.POST("/bots/:bot_id/ack", rt.ackBot)                    // ack provision result
		daemon.GET("/providers", rt.listProviders)                     // active runtime-provider catalog
		daemon.POST("/upgrades/:task_id/report", rt.upgradeReport)     // report upgrade progress
	}

	web := r.Group("/v1", auth.Middleware("web"))
	{
		web.GET("/runtimes", rt.list)
		web.DELETE("/runtimes/:runtime_id", rt.deleteRuntime)
		web.POST("/upgrades", rt.upgradeInit)
		web.GET("/upgrades/:task_id", rt.upgradeGet)
		web.POST("/bots", rt.createBot)
		web.POST("/bots/:bot_id/mint", rt.patchBotMint)
		web.GET("/bots", rt.listBots)
		web.GET("/bots/:bot_id", rt.getBot)
		web.DELETE("/bots/:bot_id", rt.archiveBot)
	}

	// runtime_latest_versions 写入口权限高(能改 daemon 升级产物来源),用专用
	// admin token 鉴权,不与 daemon/web scope 共用。
	admin := r.Group("/v1", rt.runtimeAdminTokenAuth())
	{
		admin.POST("/runtime_latest_versions", rt.upsertLatestVersionAdmin)
	}
}

// register godoc
// @Summary      Register daemon runtimes
// @Description  Register (upsert) this daemon's detected runtimes. Idempotent; disabled/unknown providers are dropped server-side.
// @Tags         runtime
// @ID           runtime.register
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        body body registerReq true "Daemon + runtimes to register"
// @Success      200 {object} envelope.Data[registerResp] "registered runtimes"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtimes [post]
func (rt *Runtime) register(c *wkhttp.Context) {
	var req registerReq
	if err := c.BindJSON(&req); err != nil {
		rt.Error("bind register request", zap.Error(err))
		responseError(c, errcode.Validation)
		return
	}
	if req.DaemonID == "" {
		responseError(c, errcode.Validation)
		return
	}

	// Server-side clamp on daemon-reported heartbeat interval. 0 means
	// "use sweeper default" (see runSweeper). Below 1s would race the
	// sweeper; above 5min undermines stale detection entirely. Out-of-
	// range values fall back to 0 (=default) rather than erroring the
	// register — a misbehaving daemon should still register.
	const minHeartbeatIntervalMs = 1000
	const maxHeartbeatIntervalMs = 300000
	if req.HeartbeatIntervalMs != 0 &&
		(req.HeartbeatIntervalMs < minHeartbeatIntervalMs || req.HeartbeatIntervalMs > maxHeartbeatIntervalMs) {
		rt.Warn("daemon-reported heartbeat_interval_ms out of range, falling back to default",
			zap.String("daemon_id", req.DaemonID),
			zap.Int64("reported_ms", req.HeartbeatIntervalMs),
			zap.Int64("min_ms", minHeartbeatIntervalMs),
			zap.Int64("max_ms", maxHeartbeatIntervalMs),
		)
		req.HeartbeatIntervalMs = 0
	}

	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	var registered []registeredRuntimeResp

	for _, r := range req.Runtimes {
		if r.Type == "" {
			continue
		}
		// 去 codex/hermes 的服务端权威过滤:老 daemon 仍探测/上报 disabled
		// provider 时,fleet 直接丢弃不写 agent_runtime(不依赖 daemon 升级)。
		if !rt.providers.IsActiveKind(r.Type) {
			rt.Info("skip disabled/unknown provider on register",
				zap.String("provider", r.Type), zap.String("daemon_id", req.DaemonID))
			continue
		}
		status := r.Status
		if status == "" {
			status = "online"
		}

		metaMap := map[string]interface{}{
			"cli_version": req.CLIVersion,
		}
		if len(r.Agents) > 0 {
			metaMap["agents"] = r.Agents
		}
		if len(r.Plugins) > 0 {
			metaMap["plugins"] = r.Plugins
		}
		metaBytes, _ := json.Marshal(metaMap)

		m := &agentRuntimeModel{
			SpaceID:             spaceID,
			DaemonID:            req.DaemonID,
			Name:                r.Name,
			Provider:            r.Type,
			RuntimeMode:         "local",
			Status:              status,
			Version:             r.Version,
			DeviceName:          req.DeviceName,
			DeviceInfo:          req.DeviceInfo,
			Metadata:            string(metaBytes),
			OwnerUID:            ownerUID,
			HeartbeatIntervalMs: req.HeartbeatIntervalMs,
		}

		id, err := rt.db.upsert(m)
		if err != nil {
			rt.Error("upsert runtime failed", zap.Error(err), zap.String("provider", r.Type))
			responseError(c, errcode.InternalError)
			return
		}

		registered = append(registered, registeredRuntimeResp{
			ID:       id,
			Provider: r.Type,
		})

		// 插件升级关单：用本次 upsert 返回的 id + 插件版本，匹配任务 metadata.runtime_id
		// v3.3.1 §C.2: pass spaceID + ownerUID so a cross-owner daemon_id
		// collision can't complete the wrong owner's upgrade task.
		for _, p := range r.Plugins {
			if p.Name != "" && p.Version != "" {
				rt.db.completeUpgradeIfMatchedWithRuntime(req.DaemonID, spaceID, ownerUID, p.Name, p.Version, id)
			}
		}

		// Provider 组件升级关单（claude/openclaw 等 active provider）：
		// 按 (space, daemon_id, owner, provider, version, runtime_id) 匹配.
		// 服务端 upgradeInit 只允许 active provider（registry IsActiveKind）创建任务，
		// 这里无需再次过滤：非 active provider 根本不会有对应 in-progress 任务可以关。
		if r.Version != "" {
			rt.db.completeUpgradeIfMatchedWithRuntime(req.DaemonID, spaceID, ownerUID, r.Type, r.Version, id)
		}
	}

	rt.Info("daemon registered",
		zap.String("daemon_id", req.DaemonID),
		zap.String("owner", ownerUID),
		zap.Int("runtime_count", len(registered)),
	)

	// 升级关单：注册成功后检查是否有匹配的升级任务. v3.3.1 §C.2: scoped
	// by (space, owner) so cross-owner same-daemon_id can't complete
	// another owner's daemon-cli upgrade.
	if req.CLIVersion != "" {
		rt.db.completeUpgradeIfMatched(req.DaemonID, spaceID, ownerUID, "octo-daemon", req.CLIVersion)
	}

	ResponseData(c, registerResp{Runtimes: registered})
}

type providerInfo struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	BinaryName        string `json:"binary_name"`
	UpgradeTimeoutSec int    `json:"upgrade_timeout_sec"`
}

// providersResp is the listProviders payload (R1 single-object envelope).
type providersResp struct {
	Providers []providerInfo `json:"providers"`
}

// listProviders godoc
// @Summary      List active runtime providers
// @Description  Returns the active runtime-provider catalog (claude / openclaw / ...) a daemon may run. Read-only.
// @Tags         provider
// @ID           provider.list
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Success      200 {object} envelope.Data[providersResp] "active providers"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /providers [get]
func (rt *Runtime) listProviders(c *wkhttp.Context) {
	snap := rt.providers.current()
	out := make([]providerInfo, 0)
	for _, name := range snap.ActiveNames() {
		d := snap.byName[name]
		out = append(out, providerInfo{
			Name: d.Name, DisplayName: d.DisplayName,
			BinaryName: d.BinaryName, UpgradeTimeoutSec: d.UpgradeTimeoutSec,
		})
	}
	ResponseData(c, providersResp{Providers: out})
}

type upsertLatestVersionReq struct {
	Component     string          `json:"component"`
	LatestVersion string          `json:"latest_version"`
	ReleaseMeta   json.RawMessage `json:"release_meta"` // 可选 JSON 对象(daemon 自升级需要 assets+checksum)
}

// upsertLatestVersionAdmin godoc
// @Summary      Upsert a component's latest version
// @Description  Admin-only: register the latest version + optional release metadata for a component (daemon / plugin / active provider). Authenticated by the X-Runtime-Admin-Token header, not a JWT.
// @Tags         upgrade
// @ID           runtime_latest_version.upsert
// @Accept       json
// @Produce      json
// @Param        X-Runtime-Admin-Token header string true "Admin token"
// @Param        body body upsertLatestVersionReq true "Component version + optional release metadata"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "upserted"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtime_latest_versions [post]
func (rt *Runtime) upsertLatestVersionAdmin(c *wkhttp.Context) {
	var req upsertLatestVersionReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}
	// component 白名单:daemon / 插件(octo + cc-octo) / active provider
	valid := req.Component == componentDaemon || isPluginComponent(req.Component) || rt.providers.IsActiveKind(req.Component)
	if !valid {
		responseError(c, errcode.Validation)
		return
	}
	if !semverLike(req.LatestVersion) {
		responseError(c, errcode.Validation)
		return
	}
	// release_meta 可选:nil(省略)和 JSON "null" 都视为无;非空必须是合法 JSON。
	hasReleaseMeta := req.ReleaseMeta != nil && string(req.ReleaseMeta) != "null"
	if hasReleaseMeta && !json.Valid(req.ReleaseMeta) {
		responseError(c, errcode.Validation)
		return
	}
	releaseMeta := ""
	if hasReleaseMeta {
		releaseMeta = string(req.ReleaseMeta)
	}
	if err := rt.db.upsertLatestVersion(req.Component, req.LatestVersion, releaseMeta); err != nil {
		rt.Error("admin upsert latest version", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	rt.Info("admin upserted latest version",
		zap.String("component", req.Component), zap.String("version", req.LatestVersion))
	ResponseEmpty(c)
}

// semverLikeRe 宽松校验:可选 v 前缀、2-3 段数字、可选预发布/构建后缀。包级编译一次。
var semverLikeRe = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?([-+].+)?$`)

func semverLike(v string) bool { return semverLikeRe.MatchString(v) }

// heartbeat godoc
// @Summary      Daemon heartbeat
// @Description  Liveness tick for one runtime. Returns reverse-dispatch piggyback (pending upgrade task / bot.provision command / managed bots) for the daemon to act on this tick.
// @Tags         runtime
// @ID           runtime.heartbeat
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        runtime_id path int true "Runtime ID"
// @Success      200 {object} envelope.Data[heartbeatResp] "dispatch piggyback"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtimes/{runtime_id}/heartbeat [post]
func (rt *Runtime) heartbeat(c *wkhttp.Context) {
	runtimeID, perr := strconv.ParseInt(c.Param("runtime_id"), 10, 64)
	if perr != nil || runtimeID <= 0 {
		responseError(c, errcode.Validation)
		return
	}

	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	existing, err := rt.db.queryByID(runtimeID)
	if err != nil || existing == nil {
		responseError(c, errcode.NotFound)
		return
	}
	if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
		responseError(c, errcode.Forbidden)
		return
	}

	if err := rt.db.updateHeartbeat(runtimeID); err != nil {
		rt.Error("update heartbeat", zap.Error(err), zap.Int64("runtime_id", runtimeID))
		responseError(c, errcode.InternalError)
		return
	}

	// envelope success is implicit (top-level data); no status:ok field.
	var resp heartbeatResp

	// Atomically claim a pending upgrade task
	claimedUpgrade, _ := rt.db.claimPendingUpgrade(spaceID, existing.DaemonID, ownerUID)
	if claimedUpgrade != nil {
		resp.PendingUpgrade = &pendingUpgradeCmd{
			TaskID:        claimedUpgrade.ID,
			Component:     claimedUpgrade.Component,
			DownloadURL:   claimedUpgrade.DownloadURL,
			TargetVersion: claimedUpgrade.ToVersion,
			Checksum:      claimedUpgrade.Checksum,
			Metadata:      claimedUpgrade.Metadata,
		}
	}

	// Atomically claim a pending bot.provision command for this daemon.
	// PoC4: single composite command replaces PoC1's two-step agent.create
	// + bot.add cycle.
	claimedBot, _ := rt.db.claimPendingBotProvision(existing.DaemonID, existing.SpaceID, ownerUID, existing.Provider)
	if claimedBot != nil {
		resp.PendingCommand = rt.buildPendingBotProvision(claimedBot)
	}

	// PR-B.2: managed_bots tells the daemon which bots to poll from
	// matter on this heartbeat tick. PR-B.3 dropped the legacy
	// pending_task fallback — daemon expects matter-only dispatch now.
	// v3 §4.3: scoped to (existing.SpaceID, ownerUID) so a cross-owner
	// daemon_id collision (legal until §4.4 schema migration lands) can't
	// leak the other owner's bot inventory through this heartbeat hint.
	managed, mberr := rt.db.listActiveBotsForDaemon(existing.DaemonID, existing.SpaceID, ownerUID)
	if mberr != nil {
		rt.Warn("listActiveBotsForDaemon failed", zap.Error(mberr), zap.String("daemon_id", existing.DaemonID))
	} else if len(managed) > 0 {
		resp.ManagedBots = managed
	}

	ResponseData(c, resp)
}

// deregister godoc
// @Summary      Deregister runtimes
// @Description  Batch-mark this daemon's runtimes offline. Idempotent; unknown ids are skipped.
// @Tags         runtime
// @ID           runtime.deregister
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        body body deregisterReq true "runtime_ids to mark offline"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "deregistered"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtimes/_deregister [post]
func (rt *Runtime) deregister(c *wkhttp.Context) {
	var req deregisterReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}

	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	for _, id := range req.RuntimeIDs {
		existing, err := rt.db.queryByID(id)
		if err != nil {
			rt.Error("query runtime for deregister", zap.Error(err), zap.Int64("id", id))
			responseError(c, errcode.InternalError)
			return
		}
		if existing == nil {
			continue
		}
		if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
			responseError(c, errcode.Forbidden)
			return
		}
	}

	if err := rt.db.setOffline(req.RuntimeIDs); err != nil {
		rt.Error("deregister runtimes", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}

	rt.Info("daemon deregistered", zap.Int("count", len(req.RuntimeIDs)))
	ResponseEmpty(c)
}

// list godoc
// @Summary      List runtimes in a space
// @Description  Aggregate view for the runtime management UI: the caller's runtimes plus per-runtime / per-daemon update hints and in-progress upgrades. Single object (not paginated); the set is small (one user's devices).
// @Tags         runtime
// @ID           runtime.list
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        space_id query string true "Space ID"
// @Success      200 {object} envelope.Data[runtimesView] "runtimes + hints"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtimes [get]
func (rt *Runtime) list(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		responseError(c, errcode.Validation)
		return
	}

	loginUID := c.GetLoginUID()
	// FLEET MIGRATION: was SELECT FROM space_member (server-only table).
	// JWT issuer already validated membership at issue time; trust the
	// space_id claim instead.
	if !auth.MatchesSpace(c, spaceID) {
		responseError(c, errcode.Forbidden)
		return
	}

	models, err := rt.db.listBySpaceIDAndOwner(spaceID, loginUID, rt.providers.ActiveNames())
	if err != nil {
		rt.Error("list runtimes", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}

	list := make([]runtimeResp, 0, len(models))
	for _, m := range models {
		list = append(list, toRuntimeResp(m))
	}

	// 为 OpenClaw runtime 的 agent.routes 注入 route_infos（bot 名字 + 在线态）。
	// 必须在 versionHints 计算之前做，确保所有读 r.Metadata 的后续逻辑看到同一份。
	rt.enrichRuntimeRouteInfos(list, spaceID)

	latestVersions, err := rt.db.queryLatestVersions()
	if err != nil {
		rt.Warn("query latest versions failed (table may not exist)", zap.Error(err))
	}

	// Build version hints per runtime_id
	versionHints := make(map[int64]versionHint)
	if latestVersions != nil {
		for _, r := range list {
			var hint versionHint
			hasHint := false

			if latest, ok := latestVersions[r.Provider]; ok && latest != "" && r.Version != "" {
				if isVersionOlder(r.Version, latest) {
					hint.HasUpdate = true
					hint.LatestVersion = latest
					hasHint = true
				}
			}

			if pluginHas, pluginLatest := computePluginHint(r.Provider, r.Metadata, latestVersions); pluginHas {
				hint.PluginHasUpdate = true
				hint.PluginLatestVersion = pluginLatest
				hasHint = true
			}

			if hasHint {
				versionHints[r.ID] = hint
			}
		}
	}

	// Build daemon version hints per daemon_id
	daemonVersionHints := make(map[string]daemonVersionHint)
	if latestVersions != nil {
		if daemonLatest, ok := latestVersions["octo-daemon"]; ok && daemonLatest != "" {
			seen := make(map[string]bool)
			for _, r := range list {
				if seen[r.DaemonID] {
					continue
				}
				seen[r.DaemonID] = true
				var meta map[string]interface{}
				if r.Metadata != "" {
					json.Unmarshal([]byte(r.Metadata), &meta)
				}
				cliVer, _ := meta["cli_version"].(string)
				if cliVer != "" && isVersionOlder(cliVer, daemonLatest) {
					daemonVersionHints[r.DaemonID] = daemonVersionHint{
						HasUpdate:     true,
						LatestVersion: daemonLatest,
						Current:       cliVer,
					}
				}
			}
		}
	}

	// 查询每个 (daemon_id, component) 最新的进行中升级任务
	// 改成数组响应，供前端按 runtime_id / daemon_id + component 恢复按钮态。
	// failed/timeout 是终态，不占 active slot —— 用户应当能立刻重新点 Upgrade
	// 重建新任务；只把真正 in-progress 的状态列进来。
	activeUpgrades := make([]activeUpgradeItem, 0)
	var upgradeTasks []upgradeTask
	rt.db.session.SelectBySql(
		`SELECT t.id, t.daemon_id, t.component, t.status, t.from_version, t.to_version, COALESCE(t.metadata,'') as metadata, COALESCE(t.error_msg,'') as error_msg
		 FROM runtime_upgrade_task t
		 INNER JOIN (
		   SELECT daemon_id, component, MAX(created_at) as max_created
		   FROM runtime_upgrade_task
		   WHERE space_id=? AND owner_uid=?
		   AND status IN ('pending','dispatched','downloading','installing','restarting')
		   GROUP BY daemon_id, component
		 ) latest ON t.daemon_id = latest.daemon_id AND t.component = latest.component AND t.created_at = latest.max_created
		 WHERE t.space_id=? AND t.owner_uid=?`,
		spaceID, loginUID, spaceID, loginUID,
	).Load(&upgradeTasks)
	for _, t := range upgradeTasks {
		item := activeUpgradeItem{
			TaskID:      t.ID,
			DaemonID:    t.DaemonID,
			Component:   t.Component,
			Status:      t.Status,
			FromVersion: t.FromVersion,
			ToVersion:   t.ToVersion,
			ErrorMsg:    t.ErrorMsg,
		}
		// 插件任务 metadata 里的 runtime_id 透出
		if t.Metadata != "" {
			var m struct {
				RuntimeID int64 `json:"runtime_id"`
			}
			if json.Unmarshal([]byte(t.Metadata), &m) == nil {
				item.RuntimeID = m.RuntimeID
			}
		}
		activeUpgrades = append(activeUpgrades, item)
	}

	ResponseData(c, runtimesView{
		Runtimes:           list,
		VersionHints:       versionHints,
		DaemonVersionHints: daemonVersionHints,
		ActiveUpgrades:     activeUpgrades,
	})
}

// deleteRuntime godoc
// @Summary      Delete a runtime
// @Description  Hard-delete a runtime the caller owns.
// @Tags         runtime
// @ID           runtime.delete
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        runtime_id path int true "Runtime ID"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "deleted"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /runtimes/{runtime_id} [delete]
func (rt *Runtime) deleteRuntime(c *wkhttp.Context) {
	idStr := c.Param("runtime_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		responseError(c, errcode.Validation)
		return
	}

	existing, err := rt.db.queryByID(id)
	if err != nil {
		rt.Error("query runtime for delete", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	if existing == nil {
		responseError(c, errcode.NotFound)
		return
	}

	loginUID := c.GetLoginUID()
	if existing.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}

	if err := rt.db.deleteByID(id); err != nil {
		rt.Error("delete runtime", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}

	rt.Info("runtime deleted", zap.Int64("id", id), zap.String("provider", existing.Provider))
	ResponseEmpty(c)
}

func isVersionOlder(current, latest string) bool {
	// "dev", "unknown", empty → always older than any real version
	if current == "dev" || current == "unknown" || current == "" {
		return latest != "" && latest != "dev" && latest != "unknown"
	}

	parse := func(v string) []int {
		v = strings.TrimPrefix(v, "v")
		for _, sep := range []string{"-", "+"} {
			if idx := strings.Index(v, sep); idx > 0 {
				v = v[:idx]
			}
		}
		parts := strings.Split(v, ".")
		nums := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			nums = append(nums, n)
		}
		return nums
	}

	c := parse(current)
	l := parse(latest)
	if c == nil || l == nil {
		return false
	}

	maxLen := len(c)
	if len(l) > maxLen {
		maxLen = len(l)
	}
	for i := 0; i < maxLen; i++ {
		cv, lv := 0, 0
		if i < len(c) {
			cv = c[i]
		}
		if i < len(l) {
			lv = l[i]
		}
		if cv < lv {
			return true
		}
		if cv > lv {
			return false
		}
	}
	return false
}

func (rt *Runtime) runSweeper() {
	// Per-runtime stale threshold = 3 × daemon-reported heartbeat_interval_ms
	// (stored on agent_runtime by register). When a daemon registers
	// against an older fleet (no column), or doesn't yet report the field,
	// markStaleOffline falls back to defaultHeartbeatIntervalMs below.
	// staleThreshold here is only used for grace + cold-start scheduling.
	const defaultHeartbeatIntervalMs int64 = 5000
	const staleThreshold = 3 * time.Duration(defaultHeartbeatIntervalMs) * time.Millisecond
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Cold-start grace: skip stale check for 2× staleThreshold after boot.
	// Without this, fleet restart can mark all daemons stale before they
	// finish their first post-restart heartbeat round.
	//
	// graceUntil uses time.Now() which carries a monotonic clock reading;
	// time.Now().Before(graceUntil) compares monotonic readings, so NTP
	// wall-clock jumps during the grace window cannot skip it short.
	graceUntil := time.Now().Add(2 * staleThreshold)

	for range ticker.C {
		if !time.Now().Before(graceUntil) {
			n, err := rt.db.markStaleOffline(defaultHeartbeatIntervalMs)
			if err != nil {
				rt.Error("sweep stale runtimes", zap.Error(err))
				continue
			}
			if n > 0 {
				rt.Info("marked stale runtimes offline", zap.Int64("count", n))
			}
		}

		gcThreshold := 7 * 24 * time.Hour
		deleted, err := rt.db.deleteStaleOffline(gcThreshold)
		if err != nil {
			rt.Error("gc offline runtimes", zap.Error(err))
			continue
		}
		if deleted > 0 {
			rt.Info("gc'd old offline runtimes", zap.Int64("count", deleted))
		}

		active := map[string]int{}
		for _, n := range rt.providers.ActiveNames() {
			active[n] = rt.providers.TimeoutSec(n)
		}
		rt.db.timeoutStaleUpgrades(active)
	}
}

func toRuntimeResp(m *agentRuntimeModel) runtimeResp {
	return runtimeResp{
		ID:          m.Id,
		SpaceID:     m.SpaceID,
		DaemonID:    m.DaemonID,
		Name:        m.Name,
		Provider:    m.Provider,
		RuntimeMode: m.RuntimeMode,
		Status:      m.Status,
		Version:     m.Version,
		DeviceName:  m.DeviceName,
		DeviceInfo:  m.DeviceInfo,
		Metadata:    m.Metadata,
		OwnerUID:    m.OwnerUID,
		LastSeenAt:  formatTime(time.Time(m.LastSeenAt)),
		CreatedAt:   formatTime(time.Time(m.CreatedAt)),
		UpdatedAt:   formatTime(time.Time(m.UpdatedAt)),
	}
}
