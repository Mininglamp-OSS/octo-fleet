package runtime

import (
	"crypto/subtle"
	"os"

	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// runtimeAdminTokenAuth guards the runtime_latest_versions write endpoint.
// That endpoint can change the download URL / checksum source for daemon
// upgrades, so it uses a dedicated token (OCTO_RUNTIME_ADMIN_TOKEN + header
// X-Runtime-Admin-Token), not the daemon/web JWT scope. Unset → fail-closed
// (reject all). 401s render the R1/R2 error envelope via abortError.
func (rt *Runtime) runtimeAdminTokenAuth() wkhttp.HandlerFunc {
	token := os.Getenv("OCTO_RUNTIME_ADMIN_TOKEN")
	if token == "" {
		rt.Warn("OCTO_RUNTIME_ADMIN_TOKEN not set — /v1/runtime_latest_versions will reject all requests")
	}
	return func(c *wkhttp.Context) {
		if token == "" {
			abortError(c, errcode.AuthRequired)
			return
		}
		hdr := c.GetHeader("X-Runtime-Admin-Token")
		if subtle.ConstantTimeCompare([]byte(hdr), []byte(token)) != 1 {
			abortError(c, errcode.AuthRequired)
			return
		}
		c.Next()
	}
}
