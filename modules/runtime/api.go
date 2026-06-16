package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-fleet/internal/auth"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
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
	daemon := r.Group("/v1/daemon", auth.Middleware("daemon"))
	{
		daemon.POST("/register", rt.register)
		daemon.POST("/heartbeat", rt.heartbeat)
		daemon.POST("/deregister", rt.deregister)
		daemon.POST("/ping/:ping_id", rt.pingReport)
		daemon.POST("/upgrade/:task_id", rt.upgradeReport)
		daemon.POST("/bots/:id/ack", rt.ackBot)
		// 决策三 SSE 反向派发: long-lived push 替代 heartbeat 夹带 pending.
		// 双跑期 (Phase A): SSE 优先, heartbeat pending dispatch 兜底.
		// daemon 端 dedup file 去重双跑产生的重复.
		daemon.GET("/events", rt.sseEvents)
		// 决策三 A3: bot_provision secret fetch (token 不进 SSE stream).
		daemon.GET("/bot-provisions/:command_id", rt.fetchBotProvision)
		// PR-B.3: bot_task ownership moved to octo-matter; daemons ack to
		// matter directly via POST /api/v1/internal/bot-tasks/:id/ack.
		// Pre-cleanup, stale daemons posting here hit the (now-removed)
		// ackBotTask handler, which UPDATEd fleet's local bot_task table
		// AND fired writebacks to matter — that duplicated entries or
		// fought matter's claim_token guard. v3.4 cleanup PR deleted the
		// handler; the 410 stub remains for deploy-compatibility (stale
		// daemons get an actionable 410 with migration hint).
		daemon.POST("/bot-tasks/:id/ack", rt.ackBotTaskDeprecated)
		daemon.GET("/runtime-providers", rt.listProviders)
	}

	internal := r.Group("/v1/internal", rt.internalTokenAuth())
	{
		// PR-B.3 closed off the route; v3.4 cleanup PR removed the
		// handler implementation. Only the 410 stub remains for
		// stale-daemon compatibility (see ackBotTaskDeprecated above).
		// runtime-20260601-01.sql bot_task table is intentionally NOT
		// dropped — production rows may exist, DROP TABLE needs explicit
		// data-archive evaluation (separate decision).
		internal.POST("/bot-tasks", rt.createBotTaskDeprecated)
	}

	// runtime-latest-versions 写入口权限高(能改 daemon 升级产物来源),用专用
	// admin token 鉴权,不与上面宽泛的 NOTIFY_INTERNAL_TOKEN 共用。
	runtimeAdmin := r.Group("/v1/internal", rt.runtimeAdminTokenAuth())
	{
		runtimeAdmin.POST("/runtime-latest-versions", rt.upsertLatestVersionAdmin)
	}

	authGroup := r.Group("/v1", auth.Middleware("web"))
	{
		authGroup.GET("/runtimes", rt.list)
		authGroup.DELETE("/runtimes/:id", rt.deleteRuntime)
		authGroup.POST("/runtimes/ping", rt.pingInit)
		authGroup.GET("/runtimes/ping/:ping_id", rt.pingGet)
		authGroup.POST("/runtimes/upgrade", rt.upgradeInit)
		authGroup.GET("/runtimes/upgrade/:task_id", rt.upgradeGet)
		authGroup.POST("/runtimes/bots", rt.createBot)
		// POST not PATCH because wkhttp RouterGroup wraps GET/POST/PUT/DELETE
		// but skips PATCH. /mint sub-path keeps the semantic intent.
		authGroup.POST("/runtimes/bots/:id/mint", rt.patchBotMint)
		authGroup.GET("/runtimes/bots", rt.listBots)
		authGroup.GET("/runtimes/bots/:id", rt.getBot)
		authGroup.DELETE("/runtimes/bots/:id", rt.archiveBot)
	}
}

// apiKeyInfo / authAPIKey are kept dead-coded here for one release of
// reference value. They are not wired into Route(). PR-A.2 deletes them.
type apiKeyInfo struct {
	UID     string
	SpaceID string
}

func (rt *Runtime) authAPIKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		_ = strings.HasPrefix // keep imports live in dead code path
		_ = gin.H{}
		_ = zap.Error(nil)
		_ = http.StatusUnauthorized
		c.AbortWithStatusJSON(http.StatusGone, gin.H{"msg": "authAPIKey deprecated in fleet — use JWT"})
	}
}

// POST /v1/daemon/register
func (rt *Runtime) register(c *wkhttp.Context) {
	var req registerReq
	if err := c.BindJSON(&req); err != nil {
		rt.Error("bind register request", zap.Error(err))
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.DaemonID == "" {
		c.ResponseError(errors.New("daemon_id is required"))
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
			c.ResponseError(errors.New("register failed"))
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

	c.Response(gin.H{
		"runtimes": registered,
	})
}

type providerInfo struct {
	Name              string `json:"name"`
	DisplayName       string `json:"display_name"`
	BinaryName        string `json:"binary_name"`
	UpgradeTimeoutSec int    `json:"upgrade_timeout_sec"`
}

// GET /v1/daemon/runtime-providers — daemon 拉取 active provider 列表(PR-C 消费)。
func (rt *Runtime) listProviders(c *wkhttp.Context) {
	snap := rt.providers.current()
	var out []providerInfo
	for _, name := range snap.ActiveNames() {
		d := snap.byName[name]
		out = append(out, providerInfo{
			Name: d.Name, DisplayName: d.DisplayName,
			BinaryName: d.BinaryName, UpgradeTimeoutSec: d.UpgradeTimeoutSec,
		})
	}
	c.Response(gin.H{"providers": out})
}

type upsertLatestVersionReq struct {
	Component     string          `json:"component"`
	LatestVersion string          `json:"latest_version"`
	ReleaseMeta   json.RawMessage `json:"release_meta"` // 可选 JSON 对象(daemon 自升级需要 assets+checksum)
}

// POST /v1/internal/runtime-latest-versions — 人工维护 runtime_latest_version
// (替代已停的 COS 同步器)。专用 admin token 鉴权(OCTO_RUNTIME_ADMIN_TOKEN
// + X-Runtime-Admin-Token),非通用 internal token。
func (rt *Runtime) upsertLatestVersionAdmin(c *wkhttp.Context) {
	var req upsertLatestVersionReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	// component 白名单:daemon / 插件 / active provider
	valid := req.Component == componentDaemon || req.Component == componentPlugin || rt.providers.IsActiveKind(req.Component)
	if !valid {
		c.ResponseError(fmt.Errorf("component %q not in registry/whitelist", req.Component))
		return
	}
	if !semverLike(req.LatestVersion) {
		c.ResponseError(fmt.Errorf("latest_version %q not semver-like", req.LatestVersion))
		return
	}
	// release_meta 可选:nil(省略)和 JSON "null" 都视为无;非空必须是合法 JSON。
	hasReleaseMeta := req.ReleaseMeta != nil && string(req.ReleaseMeta) != "null"
	if hasReleaseMeta && !json.Valid(req.ReleaseMeta) {
		c.ResponseError(errors.New("release_meta must be valid JSON"))
		return
	}
	releaseMeta := ""
	if hasReleaseMeta {
		releaseMeta = string(req.ReleaseMeta)
	}
	if err := rt.db.upsertLatestVersion(req.Component, req.LatestVersion, releaseMeta); err != nil {
		rt.Error("admin upsert latest version", zap.Error(err))
		c.ResponseError(errors.New("upsert failed"))
		return
	}
	rt.Info("admin upserted latest version",
		zap.String("component", req.Component), zap.String("version", req.LatestVersion))
	c.Response(gin.H{"ok": true})
}

// semverLikeRe 宽松校验:可选 v 前缀、2-3 段数字、可选预发布/构建后缀。包级编译一次。
var semverLikeRe = regexp.MustCompile(`^v?\d+\.\d+(\.\d+)?([-+].+)?$`)

func semverLike(v string) bool { return semverLikeRe.MatchString(v) }

func (rt *Runtime) heartbeat(c *wkhttp.Context) {
	var req heartbeatReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.RuntimeID <= 0 {
		c.ResponseError(errors.New("runtime_id is required"))
		return
	}

	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	existing, err := rt.db.queryByID(req.RuntimeID)
	if err != nil || existing == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}
	if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	if err := rt.db.updateHeartbeat(req.RuntimeID); err != nil {
		rt.Error("update heartbeat", zap.Error(err), zap.Int64("runtime_id", req.RuntimeID))
		c.ResponseError(errors.New("heartbeat failed"))
		return
	}

	// Atomically claim a pending ping for this daemon (prevents duplicate dispatch)
	resp := gin.H{"status": "ok"}
	claimedPing, _ := rt.db.claimPendingPing(spaceID, existing.DaemonID, ownerUID, time.Now().UnixMilli())
	if claimedPing != nil {
		resp["pending_ping"] = gin.H{
			"ping_id": claimedPing.ID,
		}
	}

	// Atomically claim a pending upgrade task
	claimedUpgrade, _ := rt.db.claimPendingUpgrade(spaceID, existing.DaemonID, ownerUID)
	if claimedUpgrade != nil {
		resp["pending_upgrade"] = gin.H{
			"task_id":        claimedUpgrade.ID,
			"component":      claimedUpgrade.Component,
			"download_url":   claimedUpgrade.DownloadURL,
			"target_version": claimedUpgrade.ToVersion,
			"checksum":       claimedUpgrade.Checksum,
			"metadata":       claimedUpgrade.Metadata,
		}
	}

	// Atomically claim a pending bot.provision command for this daemon.
	// PoC4: single composite command replaces PoC1's two-step agent.create
	// + bot.add cycle.
	claimedBot, _ := rt.db.claimPendingBotProvision(existing.DaemonID, existing.SpaceID, ownerUID, existing.Provider)
	if claimedBot != nil {
		resp["pending_command"] = rt.buildPendingBotProvision(claimedBot)
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
		resp["managed_bots"] = managed
	}

	c.Response(resp)
}

// POST /v1/daemon/deregister
func (rt *Runtime) deregister(c *wkhttp.Context) {
	var req deregisterReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}

	ownerUID := c.MustGet("uid").(string)
	spaceID := c.MustGet("space_id").(string)

	for _, id := range req.RuntimeIDs {
		existing, err := rt.db.queryByID(id)
		if err != nil {
			rt.Error("query runtime for deregister", zap.Error(err), zap.Int64("id", id))
			c.ResponseError(errors.New("query failed"))
			return
		}
		if existing == nil {
			continue
		}
		if existing.OwnerUID != ownerUID || existing.SpaceID != spaceID {
			c.ResponseErrorWithStatus(errors.New("no permission"), 403)
			return
		}
	}

	if err := rt.db.setOffline(req.RuntimeIDs); err != nil {
		rt.Error("deregister runtimes", zap.Error(err))
		c.ResponseError(errors.New("deregister failed"))
		return
	}

	rt.Info("daemon deregistered", zap.Int("count", len(req.RuntimeIDs)))
	c.ResponseOK()
}

// GET /v1/runtimes?space_id=xxx
func (rt *Runtime) list(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		c.ResponseError(errors.New("space_id is required"))
		return
	}

	loginUID := c.GetLoginUID()
	// FLEET MIGRATION: was SELECT FROM space_member (server-only table).
	// JWT issuer already validated membership at issue time; trust the
	// space_id claim instead.
	if !auth.MatchesSpace(c, spaceID) {
		c.ResponseErrorWithStatus(errors.New("no permission to access this space"), 403)
		return
	}

	models, err := rt.db.listBySpaceIDAndOwner(spaceID, loginUID, rt.providers.ActiveNames())
	if err != nil {
		rt.Error("list runtimes", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
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
	versionHints := make(map[int64]gin.H)
	if latestVersions != nil {
		for _, r := range list {
			hint := gin.H{}
			hasHint := false

			if latest, ok := latestVersions[r.Provider]; ok && latest != "" && r.Version != "" {
				if isVersionOlder(r.Version, latest) {
					hint["has_update"] = true
					hint["latest_version"] = latest
					hasHint = true
				}
			}

			if r.Provider == "openclaw" && r.Metadata != "" {
				if pluginLatest, ok := latestVersions["octo"]; ok && pluginLatest != "" {
					var meta map[string]interface{}
					if json.Unmarshal([]byte(r.Metadata), &meta) == nil {
						plugins, _ := meta["plugins"].([]interface{})
						for _, p := range plugins {
							pm, _ := p.(map[string]interface{})
							if pm["name"] == "octo" {
								pv, _ := pm["version"].(string)
								if pv != "" && isVersionOlder(pv, pluginLatest) {
									hint["plugin_has_update"] = true
									hint["plugin_latest_version"] = pluginLatest
									hasHint = true
								}
							}
						}
					}
				}
			}

			if hasHint {
				versionHints[r.ID] = hint
			}
		}
	}

	// Build daemon version hints per daemon_id
	daemonVersionHints := make(map[string]gin.H)
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
					daemonVersionHints[r.DaemonID] = gin.H{
						"has_update":     true,
						"latest_version": daemonLatest,
						"current":        cliVer,
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

	c.Response(gin.H{
		"runtimes":             list,
		"version_hints":        versionHints,
		"daemon_version_hints": daemonVersionHints,
		"active_upgrades":      activeUpgrades,
	})
}

// DELETE /v1/runtimes/:id
func (rt *Runtime) deleteRuntime(c *wkhttp.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.ResponseError(errors.New("invalid runtime id"))
		return
	}

	existing, err := rt.db.queryByID(id)
	if err != nil {
		rt.Error("query runtime for delete", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if existing == nil {
		c.ResponseError(errors.New("runtime not found"))
		return
	}

	loginUID := c.GetLoginUID()
	if existing.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission to delete this runtime"), 403)
		return
	}

	if err := rt.db.deleteByID(id); err != nil {
		rt.Error("delete runtime", zap.Error(err))
		c.ResponseError(errors.New("delete failed"))
		return
	}

	rt.Info("runtime deleted", zap.Int64("id", id), zap.String("provider", existing.Provider))
	c.ResponseOK()
}

// POST /v1/runtimes/ping — initiate ping to a daemon
func (rt *Runtime) pingInit(c *wkhttp.Context) {
	var req pingInitReq
	if err := c.BindJSON(&req); err != nil {
		c.ResponseError(errors.New("invalid request body"))
		return
	}
	if req.DaemonID == "" || req.SpaceID == "" {
		c.ResponseError(errors.New("daemon_id and space_id are required"))
		return
	}

	loginUID := c.GetLoginUID()
	// FLEET MIGRATION: trust JWT.space_id and only verify the agent_runtime
	// exists in that space (no JOIN to space_member which fleet can't see).
	if !auth.MatchesSpace(c, req.SpaceID) {
		c.ResponseErrorWithStatus(errors.New("no permission to ping this device"), 403)
		return
	}
	// 合并 plan 决策一+二 Phase 3B 补漏: pingInit 加 owner_uid 校验, 防止
	// 同 space 内 user A 让 user B 的 daemon 来 ping (拿 RTT 等数据).
	var accessCount int
	if err := rt.db.session.SelectBySql(
		`SELECT COUNT(*) FROM agent_runtime WHERE space_id = ? AND daemon_id = ? AND owner_uid = ?`,
		req.SpaceID, req.DaemonID, loginUID,
	).LoadOne(&accessCount); err != nil {
		rt.Error("check daemon access", zap.Error(err))
		c.ResponseError(errors.New("query failed"))
		return
	}
	if accessCount == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission to ping this device"), 403)
		return
	}
	_ = loginUID

	pingID := fmt.Sprintf("ping_%d", time.Now().UnixNano())
	entry := &pingEntry{
		ID:       pingID,
		DaemonID: req.DaemonID,
		SpaceID:  req.SpaceID,
		OwnerUID: loginUID, // v3.3.1 §C.1: persist owner so claim/get can scope by it
		ServerTS: time.Now().UnixMilli(),
		Status:   "pending",
	}
	if err := rt.db.insertPing(entry); err != nil {
		rt.Error("insert ping", zap.Error(err))
		c.ResponseError(errors.New("ping init failed"))
		return
	}

	// 决策三 SSE 反向派发 (Phase A 双跑): 给该 daemon 任一 runtime 推 ping
	// event, daemon 任一 runtime SSE goroutine 收到即唤起 ping handler
	// (daemon dedup 按 (event_type, ping_id) 防重复). 失败不阻塞主流程 —
	// heartbeat claimPendingPing 仍兜底.
	if rid, rerr := rt.db.firstRuntimeIDForDaemon(req.SpaceID, req.DaemonID, loginUID); rerr == nil && rid > 0 {
		rt.dispatchPing(rid, req.SpaceID, loginUID, pingID)
	} else if rerr != nil {
		rt.Warn("sse: firstRuntimeIDForDaemon (ping)", zap.Error(rerr), zap.String("daemon_id", req.DaemonID))
	}

	go func() {
		time.Sleep(30 * time.Second)
		rt.db.timeoutPing(pingID)
	}()

	c.Response(gin.H{"ping_id": pingID})
}

// POST /v1/daemon/ping/:ping_id — daemon reports ping result
func (rt *Runtime) pingReport(c *wkhttp.Context) {
	pingID := c.Param("ping_id")

	entry, err := rt.db.getPing(pingID)
	if err != nil || entry == nil {
		c.ResponseError(errors.New("ping not found"))
		return
	}

	ownerUID := c.MustGet("uid").(string)
	apiSpaceID := c.MustGet("space_id").(string)
	if entry.SpaceID != apiSpaceID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}
	// v3.3.2 #2 (Jerry-Xin three-round P0): runtime_ping now carries its
	// own owner_uid (runtime-20260606-02 migration), so reject when the
	// row's owner doesn't match the caller. The agent_runtime EXISTS
	// check below is necessary but not sufficient: after the 4-tuple
	// unique key two owners can legitimately share (space, daemon),
	// so EXISTS would pass for the wrong owner if they knew or guessed
	// the ping id. Direct ping.owner_uid compare is the authoritative
	// gate; the agent_runtime check stays as defense-in-depth (catches
	// stale runtime rows / future bugs that miss the ping owner_uid
	// plumbing). Symmetric with pingGet's v3.3.1 §C.1 (e) fix.
	if entry.OwnerUID != ownerUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}
	var daemonMatch int
	_ = rt.db.session.SelectBySql(
		`SELECT COUNT(*) FROM agent_runtime WHERE space_id=? AND daemon_id=? AND owner_uid=?`,
		entry.SpaceID, entry.DaemonID, ownerUID,
	).LoadOne(&daemonMatch)
	if daemonMatch == 0 {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	// RTT = now (server receives report) - server_ts (server dispatched via heartbeat)
	nowMS := time.Now().UnixMilli()
	rtt := nowMS - entry.ServerTS
	if rtt < 0 {
		rtt = 0
	}

	if err := rt.db.updatePingResult(pingID, nowMS, rtt); err != nil {
		rt.Error("update ping result", zap.Error(err))
		c.ResponseError(errors.New("update failed"))
		return
	}

	c.Response(gin.H{"status": "ok"})
}

// GET /v1/runtimes/ping/:ping_id — get ping result
func (rt *Runtime) pingGet(c *wkhttp.Context) {
	pingID := c.Param("ping_id")
	entry, err := rt.db.getPing(pingID)
	if err != nil || entry == nil {
		c.ResponseError(errors.New("ping not found"))
		return
	}

	loginUID := c.GetLoginUID()
	// FLEET MIGRATION: trust JWT.space_id (see auth.MatchesSpace).
	if !auth.MatchesSpace(c, entry.SpaceID) {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}
	// v3.3.1 §C.1 (Jerry-Xin Critical, three-round): the ping row now
	// carries its own owner_uid (runtime-20260606-02 schema migration),
	// so the ownership check is a direct field compare — no JOIN through
	// agent_runtime, no risk of resolving the wrong owner's row on a
	// cross-owner daemon_id collision (the v3 §4.6 COUNT-WHERE-agent_runtime
	// approach was a step in the right direction but the data model
	// gap — runtime_ping without owner_uid — still let a collision
	// produce inconsistent answers depending on which row got returned).
	// Direct compare also saves one query per ping read.
	if entry.OwnerUID != loginUID {
		c.ResponseErrorWithStatus(errors.New("no permission"), 403)
		return
	}

	c.Response(gin.H{
		"ping_id": entry.ID,
		"status":  entry.Status,
		"rtt_ms":  entry.RTT,
	})
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

		if cleaned, err := rt.db.cleanOldPings(5 * time.Minute); err != nil {
			rt.Error("clean old pings", zap.Error(err))
		} else if cleaned > 0 {
			rt.Info("cleaned old ping entries", zap.Int64("count", cleaned))
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
