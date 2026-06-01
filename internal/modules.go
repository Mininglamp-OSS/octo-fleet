// Module registry for octo-fleet.
//
// Fleet runs ONLY the runtime module (renamed-in-place from octo-server's
// modules/runtime). No user / IM / messaging modules — those stay in
// octo-server and fleet talks to them only as a JWT verifier (server's
// public key fetched once from /.well-known/jwks.json).
package modules

import (
	_ "github.com/Mininglamp-OSS/octo-fleet/modules/runtime"
)
