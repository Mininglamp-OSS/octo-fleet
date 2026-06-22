package runtime

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
)

type agentRuntimeModel struct {
	SpaceID             string
	DaemonID            string
	Name                string
	Provider            string
	RuntimeMode         string
	Status              string
	Version             string
	DeviceName          string
	DeviceInfo          string
	Metadata            string
	OwnerUID            string
	HeartbeatIntervalMs int64 // 0 = unset, sweeper falls back to default
	LastSeenAt          db.Time
	db.BaseModel
}

type registerReq struct {
	DaemonID            string       `json:"daemon_id"`
	DeviceName          string       `json:"device_name"`
	DeviceInfo          string       `json:"device_info"`
	CLIVersion          string       `json:"cli_version"`
	HeartbeatIntervalMs int64        `json:"heartbeat_interval_ms,omitempty"` // daemon-reported, 0 = unset
	Runtimes            []runtimeReq `json:"runtimes"`
}

type runtimeReq struct {
	Name    string       `json:"name"`
	Type    string       `json:"type"`
	Version string       `json:"version"`
	Status  string       `json:"status"`
	Agents  []agentInfo  `json:"agents,omitempty"`
	Plugins []pluginInfo `json:"plugins,omitempty"`
}

type pluginInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type agentInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Bindings int      `json:"bindings"`
	Default  bool     `json:"is_default"`
	Routes   []string `json:"routes,omitempty"`
}

type deregisterReq struct {
	RuntimeIDs []int64 `json:"runtime_ids"`
}

type runtimeResp struct {
	ID          int64  `json:"id"`
	SpaceID     string `json:"space_id"`
	DaemonID    string `json:"daemon_id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	RuntimeMode string `json:"runtime_mode"`
	Status      string `json:"status"`
	Version     string `json:"version"`
	DeviceName  string `json:"device_name"`
	DeviceInfo  string `json:"device_info"`
	Metadata    string `json:"metadata"`
	OwnerUID    string `json:"owner_uid"`
	LastSeenAt  string `json:"last_seen_at" swaggertype:"string,date-time"`
	CreatedAt   string `json:"created_at" swaggertype:"string,date-time"`
	UpdatedAt   string `json:"updated_at" swaggertype:"string,date-time"`
}

type registeredRuntimeResp struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
}

type upgradeInitReq struct {
	DaemonID  string `json:"daemon_id"`
	SpaceID   string `json:"space_id"`
	Component string `json:"component"`            // 默认 "octo-daemon"；插件填 "octo" / "cc-octo"
	RuntimeID int64  `json:"runtime_id,omitempty"` // 插件分支必填：对应 runtime 的 id
	// cc-octo 一键安装专用：LLM 网关 + key。仅在 cc-octo install 时必填。
	// 绝不入库 / 不进 metadata —— 受理后只进内存 transient store 中转给 daemon。
	GatewayURL string `json:"gateway_url,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
}

type upgradeReportReq struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

type activeUpgradeItem struct {
	TaskID      string `json:"task_id"`
	DaemonID    string `json:"daemon_id"`
	Component   string `json:"component"`
	RuntimeID   int64  `json:"runtime_id,omitempty"` // 插件有值，daemon 升级无值
	Status      string `json:"status"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	ErrorMsg    string `json:"error_msg"`
}

type upgradeTask struct {
	ID          string `db:"id"`
	SpaceID     string `db:"space_id"`
	DaemonID    string `db:"daemon_id"`
	OwnerUID    string `db:"owner_uid"`
	Component   string `db:"component"`
	FromVersion string `db:"from_version"`
	ToVersion   string `db:"to_version"`
	DownloadURL string `db:"download_url"`
	Checksum    string `db:"checksum"`
	Metadata    string `db:"metadata"`
	Status      string `db:"status"`
	ErrorMsg    string `db:"error_msg"`
}

type releaseMetaJSON struct {
	Tag       string             `json:"tag"`
	Assets    []releaseAssetJSON `json:"assets"`
	Checksums map[string]string  `json:"checksums"`
}

type releaseAssetJSON struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
	Kind string `json:"kind"`
}

// --- named response payloads (R1) for swag schema generation ---

// registerResp is the POST /runtimes response.
type registerResp struct {
	Runtimes []registeredRuntimeResp `json:"runtimes"`
}

// verifyResp is the POST /runtimes/verify response — the owner + space the
// daemon's api_key resolved to (echoed from the auth middleware).
type verifyResp struct {
	SpaceID  string `json:"space_id"`
	OwnerUID string `json:"owner_uid"`
}

// pendingUpgradeCmd is a heartbeat-piggybacked upgrade task for the daemon.
type pendingUpgradeCmd struct {
	TaskID        string `json:"task_id"`
	Component     string `json:"component"`
	DownloadURL   string `json:"download_url" swaggertype:"string,uri"`
	TargetVersion string `json:"target_version"`
	Checksum      string `json:"checksum"`
	Metadata      string `json:"metadata"`
}

// botProvisionCmd is the heartbeat-piggybacked bot.provision command.
// Like botProvisionFetchResponse it intentionally OMITS bot_token — the
// token stays on octo-server and the daemon fetches it separately
// (GET /v1/bot/:uid/token). Fleet never puts the token on the wire.
type botProvisionCmd struct {
	ID          int64  `json:"id"`
	Action      string `json:"action"`
	RuntimeKind string `json:"runtime_kind"`
	WorkspaceID string `json:"workspace_id"`
	DisplayName string `json:"display_name"`
	BotUID      string `json:"bot_uid"`
	ClaimToken  string `json:"claim_token"`
}

// managedBot is one entry in heartbeat managed_bots — bots the daemon must
// poll matter for. Also the row type for listActiveBotsForDaemon.
type managedBot struct {
	BotUID      string `json:"bot_uid" db:"bot_uid"`
	WorkspaceID string `json:"workspace_id" db:"workspace_id"`
}

// heartbeatResp is the POST /runtimes/{runtime_id}/heartbeat response —
// reverse-dispatch piggybacked on the daemon's liveness tick.
type heartbeatResp struct {
	PendingUpgrade *pendingUpgradeCmd `json:"pending_upgrade,omitempty"`
	PendingCommand *botProvisionCmd   `json:"pending_command,omitempty"`
	ManagedBots    []managedBot       `json:"managed_bots,omitempty"`
}

// versionHint flags an available update for one runtime (per runtime_id).
type versionHint struct {
	HasUpdate           bool   `json:"has_update,omitempty"`
	LatestVersion       string `json:"latest_version,omitempty"`
	PluginHasUpdate     bool   `json:"has_plugin_update,omitempty"`
	PluginLatestVersion string `json:"plugin_latest_version,omitempty"`
}

// daemonVersionHint flags an available daemon (CLI) update (per daemon_id).
type daemonVersionHint struct {
	HasUpdate     bool   `json:"has_update,omitempty"`
	LatestVersion string `json:"latest_version,omitempty"`
	Current       string `json:"current,omitempty"`
}

// runtimesView is the GET /runtimes aggregate (list + per-id/per-daemon
// update hints + in-progress upgrades). Single-object envelope, not paginated.
type runtimesView struct {
	Runtimes           []runtimeResp                `json:"runtimes"`
	VersionHints       map[int64]versionHint        `json:"version_hints"`
	DaemonVersionHints map[string]daemonVersionHint `json:"daemon_version_hints"`
	ActiveUpgrades     []activeUpgradeItem          `json:"active_upgrades"`
}

// upgradeInitResp is the POST /upgrades response.
type upgradeInitResp struct {
	TaskID string `json:"task_id"`
}

// upgradeGetResp is the GET /upgrades/{task_id} response.
type upgradeGetResp struct {
	ID          string `json:"id"`
	Component   string `json:"component"`
	Status      string `json:"status"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	ErrorMsg    string `json:"error_msg"`
}
