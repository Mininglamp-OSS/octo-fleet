package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-fleet/internal/auth"
	_ "github.com/Mininglamp-OSS/octo-fleet/internal/envelope" // swag @Success/@Failure type resolution
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"go.uber.org/zap"
)

// ---------- bot model ----------

type botModel struct {
	Id          int64
	SpaceID     string  `db:"space_id"`
	OwnerUID    string  `db:"owner_uid"`
	RuntimeID   int64   `db:"runtime_id"`
	RuntimeKind string  `db:"runtime_kind"`
	DaemonID    string  `db:"daemon_id"`
	Name        string  `db:"name"`
	BotUID      string  `db:"bot_uid"`
	BotToken    string  `db:"bot_token"`
	WorkspaceID string  `db:"workspace_id"`
	Status      string  `db:"status"`
	ClaimToken  string  `db:"claim_token"`
	ErrorMsg    string  `db:"error_msg"`
	CreatedBy   string  `db:"created_by"`
	CreatedAt   db.Time `db:"created_at"`
	UpdatedAt   db.Time `db:"updated_at"`
}

const botSelectColumns = "id, space_id, owner_uid, runtime_id, runtime_kind, daemon_id, name, bot_uid, bot_token, workspace_id, status, claim_token, error_msg, created_by, created_at, updated_at"

const (
	botStatusDraft        = "draft"
	botStatusProvisioning = "provisioning"
	botStatusBotMinted    = "bot_minted"
	botStatusDispatched   = "dispatched"
	botStatusActive       = "active"
	botStatusFailed       = "failed"
	botStatusArchived     = "archived"
)

// ---------- request / response ----------

type createBotReq struct {
	RuntimeID   int64  `json:"runtime_id"`
	Name        string `json:"name"`
	RuntimeKind string `json:"runtime_kind"`
}

type botResp struct {
	ID          int64  `json:"id"`
	SpaceID     string `json:"space_id"`
	OwnerUID    string `json:"owner_uid"`
	RuntimeID   int64  `json:"runtime_id"`
	RuntimeKind string `json:"runtime_kind"`
	DaemonID    string `json:"daemon_id"`
	Name        string `json:"name"`
	BotUID      string `json:"bot_uid"`
	WorkspaceID string `json:"workspace_id"`
	Status      string `json:"status"`
	ErrorMsg    string `json:"error_msg,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type ackBotReq struct {
	ClaimToken string `json:"claim_token"`
	Status     string `json:"status"`
	ErrorMsg   string `json:"error_msg,omitempty"`
}

func toBotResp(m *botModel) botResp {
	return botResp{
		ID:          m.Id,
		SpaceID:     m.SpaceID,
		OwnerUID:    m.OwnerUID,
		RuntimeID:   m.RuntimeID,
		RuntimeKind: m.RuntimeKind,
		DaemonID:    m.DaemonID,
		Name:        m.Name,
		BotUID:      m.BotUID,
		WorkspaceID: m.WorkspaceID,
		Status:      m.Status,
		ErrorMsg:    m.ErrorMsg,
		CreatedAt:   formatTime(time.Time(m.CreatedAt)),
		UpdatedAt:   formatTime(time.Time(m.UpdatedAt)),
	}
}

// ---------- db helpers ----------

func (d *runtimeDB) insertBot(m *botModel) (int64, error) {
	res, err := d.session.InsertBySql(
		`INSERT INTO bot (space_id, owner_uid, runtime_id, runtime_kind, daemon_id,
		                  name, bot_uid, bot_token, workspace_id, status, error_msg, created_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'poc')`,
		m.SpaceID, m.OwnerUID, m.RuntimeID, m.RuntimeKind, m.DaemonID,
		m.Name, m.BotUID, m.BotToken, m.WorkspaceID, m.Status, m.ErrorMsg,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *runtimeDB) updateBotStatus(id int64, status, errMsg string) error {
	_, err := d.session.UpdateBySql(
		`UPDATE bot SET status=?, error_msg=? WHERE id=?`,
		status, errMsg, id,
	).Exec()
	return err
}

// ackBotStatus atomically flips a *dispatched* bot to the acked status
// and clears its claim_token, gated on a matching claim_token. The
// `status=dispatched AND claim_token=?` guard makes a delayed or
// duplicated daemon ack against an archived/terminal bot a no-op (zero
// rows), and clearing the token stops a second ack with the same token
// from ever matching again. Returns rows affected so the caller can tell
// a real flip from a replayed/stale ack.
//
// This is the actual anti-replay enforcement: the handler's query-time
// space/owner/token checks are friendly 4xx guards, but the
// queryBotByID→update window is racy, so correctness can't rely on them.
func (d *runtimeDB) ackBotStatus(id int64, status, errMsg, claimToken string) (int64, error) {
	res, err := d.session.UpdateBySql(
		`UPDATE bot SET status=?, error_msg=?, claim_token='' WHERE id=? AND status=? AND claim_token=?`,
		status, errMsg, id, botStatusDispatched, claimToken,
	).Exec()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (d *runtimeDB) queryBotByID(id int64) (*botModel, error) {
	var m botModel
	count, err := d.session.SelectBySql(
		"SELECT "+botSelectColumns+" FROM bot WHERE id=?", id,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

// listBotsBySpace lists active bots in the given (space, owner) scope.
// v3 §4.5 (defense-in-depth C): ownerUID is now mandatory — the prior
// "ownerUID=” disables filter" branch was the enumeration vector via
// listBots's `?owner_uid=` query param. Caller must always pass the
// authenticated loginUID; pass-through of unauthenticated input is no
// longer supported here.
func (d *runtimeDB) listBotsBySpace(spaceID, ownerUID, runtimeKind string, activeKinds []string, page, pageSize int) ([]*botModel, int64, error) {
	if ownerUID == "" {
		return nil, 0, errors.New("listBotsBySpace: ownerUID required (v3 §4.5)")
	}
	// active 为空 = 没有任何 active provider,返回空(而非退化成列出全部 bot)。
	if len(activeKinds) == 0 {
		return []*botModel{}, 0, nil
	}
	where := `FROM bot
		 WHERE space_id=? AND status != ? AND owner_uid=?
		   AND (?='' OR runtime_kind=?)`
	args := []interface{}{spaceID, botStatusArchived, ownerUID, runtimeKind, runtimeKind}
	if len(activeKinds) > 0 {
		ph := strings.Repeat("?,", len(activeKinds))
		ph = ph[:len(ph)-1]
		where += " AND runtime_kind IN (" + ph + ")"
		for _, k := range activeKinds {
			args = append(args, k)
		}
	}
	var total int64
	if _, err := d.session.SelectBySql("SELECT COUNT(*) "+where, args...).Load(&total); err != nil {
		return nil, 0, err
	}
	sql := "SELECT " + botSelectColumns + " " + where + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)
	var out []*botModel
	if _, err := d.session.SelectBySql(sql, args...).Load(&out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// claimPendingBotProvision picks one bot_minted row of the given runtime kind
// for this daemon, marks dispatched + sets claim_token, returns it.
func (d *runtimeDB) claimPendingBotProvision(daemonID, spaceID, ownerUID, runtimeKind string) (*botModel, error) {
	tx, err := d.session.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.RollbackUnlessCommitted()

	var m botModel
	// (owner_uid, space_id) filter (reviewer fleet#24 Jerry-Xin three-
	// round): bot table has owner_uid + space_id; without both filters
	// user B could claim a bot provision row that was inserted for user
	// A's daemon (same daemon_id, same space), OR a daemon in Space B
	// could claim a bot provision row from Space A for the same owner —
	// both legal collision shapes after runtime-20260606-01 4-tuple
	// unique key. v3 §4.3 added owner_uid; v3.3.2 #1 closes the missing
	// space_id dimension (api_keys are space-bound, so a same-owner
	// cross-space daemon is a valid collision target).
	count, err := tx.SelectBySql(
		"SELECT "+botSelectColumns+` FROM bot
		 WHERE daemon_id=? AND owner_uid=? AND space_id=? AND runtime_kind=? AND status=?
		 ORDER BY id ASC LIMIT 1 FOR UPDATE`,
		daemonID, ownerUID, spaceID, runtimeKind, botStatusBotMinted,
	).Load(&m)
	if err != nil || count == 0 {
		return nil, err
	}
	token := randomToken()
	if _, err := tx.UpdateBySql(
		`UPDATE bot SET status=?, claim_token=? WHERE id=?`,
		botStatusDispatched, token, m.Id,
	).Exec(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.Status = botStatusDispatched
	m.ClaimToken = token
	return &m, nil
}

// resolveBotByUID looks up a bot by its bot_uid (called by bot_task
// dispatch to find workspace_id + daemon_id + runtime_kind for a matter
// assignee). Replaces the old managed_runtime_agent reverse lookup.
func (d *runtimeDB) resolveBotByUID(spaceID, botUID string) (*botModel, error) {
	var m botModel
	count, err := d.session.SelectBySql(
		"SELECT "+botSelectColumns+` FROM bot
		 WHERE space_id=? AND bot_uid=? AND status!=?
		 ORDER BY id DESC LIMIT 1`,
		spaceID, botUID, botStatusArchived,
	).Load(&m)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return &m, nil
}

// ---------- helpers ----------

var workspaceSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// deriveWorkspaceID turns the user's bot name into an openclaw workspace
// slug. We always append a short random suffix so two bots named "dev"
// don't collide on the daemon. Workspace is internal — user never sees it.
func deriveWorkspaceID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = workspaceSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "bot"
	}
	if len(s) > 28 {
		s = s[:28]
	}
	suf := make([]byte, 2)
	_, _ = rand.Read(suf)
	return s + "-" + hex.EncodeToString(suf)
}

// ---------- HTTP handlers ----------

// createBot godoc
// @Summary      Create a bot
// @Description  Create a draft bot on a runtime the caller owns. Returns the draft; mint it via POST /bots/{bot_id}/mint to start provisioning.
// @Tags         bot
// @ID           bot.create
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        body body createBotReq true "runtime_id, name, runtime_kind"
// @Success      201 {object} envelope.Data[botResp] "draft bot"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /bots [post]
func (rt *Runtime) createBot(c *wkhttp.Context) {
	var req createBotReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}
	if req.RuntimeID <= 0 {
		responseError(c, errcode.Validation)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		responseError(c, errcode.Validation)
		return
	}
	if !rt.providers.IsActiveKind(req.RuntimeKind) {
		responseError(c, errcode.Validation)
		return
	}

	loginUID := c.GetLoginUID()
	runtime, err := rt.db.queryByID(req.RuntimeID)
	if err != nil || runtime == nil {
		responseError(c, errcode.NotFound)
		return
	}
	if runtime.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}
	if runtime.Provider != req.RuntimeKind {
		responseError(c, errcode.Validation)
		return
	}

	row := &botModel{
		SpaceID:     runtime.SpaceID,
		OwnerUID:    loginUID,
		RuntimeID:   req.RuntimeID,
		RuntimeKind: req.RuntimeKind,
		DaemonID:    runtime.DaemonID,
		Name:        name,
		Status:      botStatusDraft,
	}
	// Every runtime kind gets a workspace_id: the daemon's provision handler
	// requires it non-empty. openclaw uses it as the agent/workspace name;
	// other kinds (claude/codex/hermes) carry it as a stable per-bot id even
	// when their adapter doesn't key local resources on it.
	row.WorkspaceID = deriveWorkspaceID(name)

	id, err := rt.db.insertBot(row)
	if err != nil {
		rt.Error("insert bot", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	row.Id = id

	// FLEET MIGRATION: minting now happens out-of-band — the web client
	// is expected to POST octo-server /v1/bot/mint, receive {bot_uid},
	// then POST /v1/bots/{bot_id}/mint here to set the credentials
	// and trigger bot.provision. This row stays in `draft` status until
	// that patch arrives.
	_ = generateBotToken // helper retained for legacy callers
	row.CreatedAt = db.Time(time.Now())
	row.UpdatedAt = row.CreatedAt
	ResponseCreated(c, toBotResp(row))
}

// patchBotMintReq is the body of POST /v1/bots/{bot_id}/mint.
// Web supplies bot_uid (from server) — bot_token is fetched by daemon
// directly from server, NOT passed through here, so fleet never stores
// the token.
type patchBotMintReq struct {
	BotUID string `json:"bot_uid"`
}

// POST /v1/bots/{bot_id}/mint
//
// Web caller flow:
//  1. POST /v1/bots                → fleet inserts draft row, returns id
//  2. POST /v1/bot/mint (server)   → server mints IM bot, returns bot_uid
//  3. POST /v1/bots/{bot_id}/mint {bot_uid} → fleet promotes row
//     to bot_minted (openclaw) or active (inert), queues bot.provision
//     for the daemon to claim on its next heartbeat.
//
// bot_token is NEVER written to fleet — it remains on octo-server and the
// daemon fetches it via GET /v1/bot/:uid/token using its daemon-scope JWT.
//
// patchBotMint godoc
// @Summary      Mint a bot
// @Description  Attach the server-minted bot_uid to a draft bot and start provisioning (openclaw) or activate it. Idempotent only from draft state.
// @Tags         bot
// @ID           bot.mint
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        bot_id path int true "Bot ID"
// @Param        body body patchBotMintReq true "bot_uid from server mint"
// @Success      200 {object} envelope.Data[botResp] "minted bot"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      409 {object} envelope.Error "CONFLICT"
// @Router       /bots/{bot_id}/mint [post]
func (rt *Runtime) patchBotMint(c *wkhttp.Context) {
	idStr := c.Param("bot_id")
	id, perr := strconv.ParseInt(idStr, 10, 64)
	if perr != nil || id <= 0 {
		responseError(c, errcode.Validation)
		return
	}
	loginUID := c.GetLoginUID()

	var req patchBotMintReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}
	if strings.TrimSpace(req.BotUID) == "" {
		responseError(c, errcode.Validation)
		return
	}

	row, err := rt.db.queryBotByID(id)
	if err != nil || row == nil {
		responseError(c, errcode.NotFound)
		return
	}
	if row.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}
	if row.Status != botStatusDraft {
		responseError(c, errcode.Conflict)
		return
	}

	// All runtime kinds need daemon provisioning now. The daemon resolves the
	// runtime_kind to the matching adapter; kinds whose adapter is not yet
	// implemented ack the provision as failed (visible) rather than silently
	// staying inert.
	nextStatus := botStatusBotMinted
	if _, err := rt.db.session.UpdateBySql(
		`UPDATE bot SET bot_uid=?, status=? WHERE id=?`,
		req.BotUID, nextStatus, id,
	).Exec(); err != nil {
		rt.Error("patchBotMint update", zap.Error(err), zap.Int64("id", id))
		responseError(c, errcode.InternalError)
		return
	}
	row.BotUID = req.BotUID
	row.Status = nextStatus
	row.UpdatedAt = db.Time(time.Now())

	// 决策三 SSE 反向派发 (Phase A 双跑): bot 进 bot_minted 状态后, 推
	// bot_provision wake-up event 给目标 runtime, daemon 收到后走
	// GET /v1/bots/{bot_id}/provision 单独 fetch full payload
	// (workspace_id/bot_uid/claim_token, 永不含 bot_token).
	// A3 决策: token 不进 SSE stream / 不进 event_log. heartbeat
	// claimPendingBotProvision 仍兜底.
	if nextStatus == botStatusBotMinted && row.RuntimeID > 0 {
		rt.dispatchBotProvision(row.RuntimeID, row.SpaceID, row.OwnerUID, fmt.Sprintf("%d", row.Id))
		rt.dispatchManagedBotsChanged(row.RuntimeID, row.SpaceID, row.OwnerUID, []string{req.BotUID}, nil)
	}

	ResponseData(c, toBotResp(row))
}

// listBots godoc
// @Summary      List bots
// @Description  List the caller's bots in a space (offset-paginated). Scoped to the authenticated owner.
// @Tags         bot
// @ID           bot.list
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        space_id     query string true  "Space ID"
// @Param        runtime_kind query string false "Filter by runtime kind"
// @Param        page         query int    false "Page number, default 1"
// @Param        page_size    query int    false "Page size, default 20, max 100"
// @Success      200 {object} envelope.OffsetList[botResp] "bots"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /bots [get]
func (rt *Runtime) listBots(c *wkhttp.Context) {
	spaceID := c.Query("space_id")
	if spaceID == "" {
		responseError(c, errcode.Validation)
		return
	}
	loginUID := c.GetLoginUID()
	// v2 鉴权关系数据补全 + v3 §4.5 (defense-in-depth C): MatchesSpace
	// compares ?space_id against the ctx space_id that the wrapper
	// injected from X-Space-Id. The wrapper validated X-Space-Id against
	// server-validated spaces (auth/middleware.go Middleware()) when
	// context_included=true; on pre-v2 fallback, the wrapper trusts the
	// header and this handler MUST NOT rely on MatchesSpace alone — hence
	// the unconditional owner_uid=loginUID filter below.
	if !auth.MatchesSpace(c, spaceID) {
		responseError(c, errcode.Forbidden)
		return
	}
	// v3 §4.5 (defense-in-depth C, yujiawei P1): owner_uid is now
	// MANDATORY and bound to loginUID — the prior `?owner_uid=` query
	// param + `?='' OR owner_uid=?` SQL was the attack surface that let
	// a zero-space caller (pre-v2 fallback / future bug) enumerate
	// another owner's bots by omitting the parameter. Sharing a space
	// with other users no longer implies seeing their bot inventory;
	// the team-shared-view UX moves to a future dedicated endpoint if
	// product wants it back.
	kind := c.Query("runtime_kind")
	// Offset pagination (not cursor) by design: this backs a web management
	// table that needs jump-to-page + a total count — exactly the R5 offset
	// use case. Insert-time drift is acceptable in that UI context.
	page, pageSize := GetOffsetParams(c)

	rows, total, err := rt.db.listBotsBySpace(spaceID, loginUID, kind, rt.providers.ActiveNames(), page, pageSize)
	if err != nil {
		rt.Error("listBots", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	out := make([]botResp, 0, len(rows))
	for _, r := range rows {
		out = append(out, toBotResp(r))
	}
	ResponseOffset(c, out, total, page, pageSize)
}

// getBot godoc
// @Summary      Get a bot
// @Description  Read a single bot the caller owns.
// @Tags         bot
// @ID           bot.get
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        bot_id path int true "Bot ID"
// @Success      200 {object} envelope.Data[botResp] "bot"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Router       /bots/{bot_id} [get]
func (rt *Runtime) getBot(c *wkhttp.Context) {
	idStr := c.Param("bot_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		responseError(c, errcode.Validation)
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil {
		rt.Error("getBot query", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	if m == nil {
		responseError(c, errcode.NotFound)
		return
	}
	loginUID := c.GetLoginUID()
	if m.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}
	ResponseData(c, toBotResp(m))
}

// archiveBot godoc
// @Summary      Archive a bot
// @Description  Soft-delete (status=archived) a bot the caller owns; emits a managed_bots_changed removal to its daemon.
// @Tags         bot
// @ID           bot.archive
// @Accept       json
// @Produce      json
// @Security     SessionToken
// @Param        bot_id path int true "Bot ID"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "archived"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      500 {object} envelope.Error "INTERNAL_ERROR"
// @Router       /bots/{bot_id} [delete]
func (rt *Runtime) archiveBot(c *wkhttp.Context) {
	idStr := c.Param("bot_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		responseError(c, errcode.Validation)
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil || m == nil {
		responseError(c, errcode.NotFound)
		return
	}
	loginUID := c.GetLoginUID()
	if m.OwnerUID != loginUID {
		responseError(c, errcode.Forbidden)
		return
	}
	if err := rt.db.updateBotStatus(id, botStatusArchived, ""); err != nil {
		rt.Error("archiveBot", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	// 决策三 SSE: bot 归档 → managed_bots_changed delta 推到目标 runtime
	// 让 daemon 立即从本地 active 集合移除. heartbeat managed_bots
	// snapshot 仍是 baseline (Phase B 改 30s 后这条 delta 是主要实时性
	// 来源 — v6 plan §3.5 A2 trade-off).
	if m.RuntimeID > 0 && m.BotUID != "" {
		rt.dispatchManagedBotsChanged(m.RuntimeID, m.SpaceID, m.OwnerUID, nil, []string{m.BotUID})
	}
	ResponseEmpty(c)
}

// ackBot godoc
// @Summary      Ack bot provision
// @Description  Daemon reports the provision result (active / failed). claim_token must match the dispatched token. On failed, emits a managed_bots_changed removal.
// @Tags         bot
// @ID           bot.ack
// @Accept       json
// @Produce      json
// @Security     Bearer
// @Param        bot_id path int true "Bot ID"
// @Param        body body ackBotReq true "claim_token + status"
// @Success      200 {object} envelope.Data[envelope.EmptyResp] "acked"
// @Failure      400 {object} envelope.Error "VALIDATION_ERROR"
// @Failure      401 {object} envelope.Error "AUTH_REQUIRED"
// @Failure      403 {object} envelope.Error "FORBIDDEN"
// @Failure      404 {object} envelope.Error "NOT_FOUND"
// @Failure      409 {object} envelope.Error "CONFLICT"
// @Router       /bots/{bot_id}/ack [post]
func (rt *Runtime) ackBot(c *wkhttp.Context) {
	idStr := c.Param("bot_id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		responseError(c, errcode.Validation)
		return
	}
	var req ackBotReq
	if err := c.BindJSON(&req); err != nil {
		responseError(c, errcode.Validation)
		return
	}
	if req.Status != botStatusActive && req.Status != botStatusFailed {
		responseError(c, errcode.Validation)
		return
	}
	m, err := rt.db.queryBotByID(id)
	if err != nil || m == nil {
		responseError(c, errcode.NotFound)
		return
	}
	spaceID := c.MustGet("space_id").(string)
	if m.SpaceID != spaceID {
		responseError(c, errcode.Forbidden)
		return
	}
	// 合并 plan 决策一+二 Phase 3B 补漏: ackBot 加 owner_uid 校验 (defense
	// in depth). claim_token 防伪已经够, 但额外校验 caller (daemon) 的 uid
	// 必须等于 bot owner_uid, 防止 daemon 配错 api_key 后误改别人 bot 状态.
	ownerUID := c.MustGet("uid").(string)
	if m.OwnerUID != ownerUID {
		responseError(c, errcode.Forbidden)
		return
	}
	// 幂等短路:bot 已是目标态 → 这个 ack 之前已成功应用过。成功 ack 会清空
	// claim_token,所以重放请求带的 token 此时和 db 对不上,必须在 claim_token
	// 校验**之前**幂等返回,否则下面的 `m.ClaimToken == ""` 会把它拦成 409,
	// 而 daemon 对非 2xx 会无限重试(replay/heartbeat 兜底)。这里是 no-op
	// (不翻状态、不发 SSE),且 space/owner 已校验,安全。archived bot 的 status
	// 是 "archived",永不等于 active/failed,不会命中短路,复活仍被堵死。
	if m.Status == req.Status {
		ResponseEmpty(c)
		return
	}
	if m.ClaimToken == "" || m.ClaimToken != req.ClaimToken {
		responseError(c, errcode.Conflict)
		return
	}
	// Atomic, replay-safe flip: only a still-dispatched bot with a
	// matching claim_token is updated, and the token is cleared so the
	// same ack can't be replayed. Zero rows means the bot is no longer
	// dispatched (already acked, archived/deleted, or token rotated) —
	// a stale/replayed ack that must NOT flip status or emit SSE.
	affected, err := rt.db.ackBotStatus(id, req.Status, req.ErrorMsg, req.ClaimToken)
	if err != nil {
		rt.Error("ackBot update", zap.Error(err))
		responseError(c, errcode.InternalError)
		return
	}
	if affected == 0 {
		// daemon 的容错会主动 replay ack (ack 失败不 markDone → SSE replay /
		// heartbeat 重试),所以重复 ack 是正常流量。区分两种零行:
		//   (a) 该 ack 之前已成功应用 (bot 已是目标态) → 幂等返回 OK,
		//       否则 daemon 收非 2xx 永远 markDone 不了 → 无限重试;
		//   (b) bot 已 archived / 处于其他终态 / token 已轮换 → 真冲突 409。
		if cur, qerr := rt.db.queryBotByID(id); qerr == nil && cur != nil && cur.Status == req.Status {
			ResponseEmpty(c)
			return
		}
		responseError(c, errcode.Conflict)
		return
	}
	// F-2 (lml2468 review): ack 'failed' 时补 managed_bots_changed{removed}
	// compensating event. patchBotMint 已发 added (bot_minted → SSE delta),
	// 这里 ack failed 必须发 removed 让 daemon 缓存清掉 phantom bot, 否则
	// daemon 一直 poll matter 拿这个不存在的 bot 的 task. heartbeat snapshot
	// 5-7s 后也会 reconcile, 但 SSE delta 是优先实时性. 'active' 不发
	// removed (bot 正常 provision 成功, daemon 缓存里就该有).
	if req.Status == botStatusFailed && m.RuntimeID > 0 && m.BotUID != "" {
		rt.dispatchManagedBotsChanged(m.RuntimeID, m.SpaceID, m.OwnerUID, nil, []string{m.BotUID})
	}
	ResponseEmpty(c)
}

// buildPendingBotProvision renders the heartbeat payload for daemon.
//
// Note: api_url is intentionally NOT included here. Fleet has no reliable
// source for the IM server URL — `cfg.External.BaseURL` is fleet's own
// external URL (per octo-lib config contract), not server's. Daemon already
// resolves api_url from its own `OCTO_SERVER_URL` env / `--api-url` flag,
// which is the single source of truth for "where is the IM server".
func (rt *Runtime) buildPendingBotProvision(m *botModel) *botProvisionCmd {
	return &botProvisionCmd{
		ID:          m.Id,
		Action:      "bot.provision",
		RuntimeKind: m.RuntimeKind,
		WorkspaceID: m.WorkspaceID,
		DisplayName: m.Name,
		BotUID:      m.BotUID,
		BotToken:    m.BotToken,
		ClaimToken:  m.ClaimToken,
	}
}

// ---------- shared helpers (moved here from deleted managed_agent.go) ----------

// randomToken: 16-byte hex string used as bot_task / bot ack claim token.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// generateBotToken mirrors the bf_xxx scheme the IM /newbot flow uses,
// so downstream Octo /v1/bot/* endpoints don't need a special-case parser.
func generateBotToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "bf_" + hex.EncodeToString(b)
}
