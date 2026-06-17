package runtime

import (
	"crypto/subtle"
	"net/http"
	"os"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// ---------- HTTP handlers ----------
//
// v3.4 cleanup (decisions 1+2 fallout): the bot_task data model and
// active HTTP handlers (createBotTask / ackBotTask / postMatterTimeline /
// postMatterActivity) were removed in this PR. bot_task ownership moved
// to octo-matter as of PR-B.3; the original code lived here in the
// PoC2 era as the matter→fleet→daemon dispatch pipeline, but the v3
// cutover replaced it with daemon→matter direct (api_key Bearer →
// matter /api/v1/internal/bot-tasks). All callers in the old chain are
// gone, the routes are 410'd (see *Deprecated below), and the orphan
// implementation was ~470 LOC of dead code. The schema table
// (runtime-20260601-01.sql `bot_task`) is left in place — DROP TABLE is
// a separate decision (production rows may still exist; needs explicit
// data-archive evaluation before scheduling a destructive migration).

// internalTokenAuth is the same shared-secret scheme used by modules/notify.
// We don't reuse the notify middleware to keep runtime independent of notify;
// both read NOTIFY_INTERNAL_TOKEN from env at process start. If the env is
// unset the middleware fails closed (rejects all requests).
//
// Used by the /v1/internal/* group, which now only mounts the
// createBotTaskDeprecated 410 stub. Kept until the stub itself is
// retired (deploy-compatibility window for stale daemons).
func (rt *Runtime) internalTokenAuth() wkhttp.HandlerFunc {
	token := os.Getenv("NOTIFY_INTERNAL_TOKEN")
	if token == "" {
		rt.Warn("NOTIFY_INTERNAL_TOKEN not set — /v1/internal/bot-tasks will reject all requests")
	}
	return func(c *wkhttp.Context) {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "internal API auth not configured"})
			return
		}
		hdr := c.GetHeader("X-Internal-Token")
		if subtle.ConstantTimeCompare([]byte(hdr), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "unauthorized"})
			return
		}
		c.Next()
	}
}

// runtimeAdminTokenAuth 守护 runtime-latest-versions 写入口。该端点能改 daemon
// 升级的下载 URL / checksum 来源,权限高于 legacy /v1/internal/* 的 410 stub,
// 故用**专用** token(OCTO_RUNTIME_ADMIN_TOKEN + header X-Runtime-Admin-Token),
// 不与宽泛的 NOTIFY_INTERNAL_TOKEN 共用。未配置则 fail-closed(拒绝全部)。
func (rt *Runtime) runtimeAdminTokenAuth() wkhttp.HandlerFunc {
	token := os.Getenv("OCTO_RUNTIME_ADMIN_TOKEN")
	if token == "" {
		rt.Warn("OCTO_RUNTIME_ADMIN_TOKEN not set — /v1/internal/runtime-latest-versions will reject all requests")
	}
	return func(c *wkhttp.Context) {
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "runtime admin API auth not configured"})
			return
		}
		hdr := c.GetHeader("X-Runtime-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(hdr), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"msg": "unauthorized"})
			return
		}
		c.Next()
	}
}
// octo-matter, fleet no longer accepts enqueue requests. Kept as a
// deploy-compatibility stub so stale daemons posting here get an
// actionable 410 (with migration hint) instead of a generic 404.
func (rt *Runtime) createBotTaskDeprecated(c *wkhttp.Context) {
	c.AbortWithStatusJSON(410, gin.H{"msg": "bot_task moved to octo-matter — POST /api/v1/internal/bot-tasks lives there now"})
}

// ackBotTaskDeprecated returns 410 Gone. PR-B.3: bot_task moved to
// octo-matter; daemon acks to matter directly via
// POST /api/v1/internal/bot-tasks/:id/ack. Kept as a deploy-
// compatibility stub for stale daemons.
func (rt *Runtime) ackBotTaskDeprecated(c *wkhttp.Context) {
	c.AbortWithStatusJSON(410, gin.H{"msg": "bot_task ack moved to octo-matter — POST /api/v1/internal/bot-tasks/:id/ack there"})
}
