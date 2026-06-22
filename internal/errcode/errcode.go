// Package errcode defines the OCTO API error code enum (spec rule R2).
//
// The wire `error.code` is one of 12 fixed values; sub-classification goes
// in `details`, never in new codes. Each Code carries its HTTP status and a
// default English message. i18n is a future increment: a localized build
// will swap Message for a lang-aware lookup keyed by Code.Code (see
// modules/runtime/errresp.go renderError) — the enum and wire shape stay put.
//
// Kept dependency-free (no gin/wkhttp) so it can lift into a shared library
// later, mirroring internal/envelope.
package errcode

import "net/http"

// Code is one R2 error code: the wire enum value, its HTTP status, and a
// default (English) message. Internal=true marks 5xx codes whose message
// and details must not leak internal cause to the client — callers log the
// underlying error before responding.
type Code struct {
	Code       string
	HTTPStatus int
	Message    string
	Internal   bool
}

// The 12 fixed R2 error codes. Pick the closest one and use details to
// sub-classify; never invent a new code.
var (
	AuthRequired         = Code{"AUTH_REQUIRED", http.StatusUnauthorized, "Authentication required.", false}
	Forbidden            = Code{"FORBIDDEN", http.StatusForbidden, "Permission denied.", false}
	NotFound             = Code{"NOT_FOUND", http.StatusNotFound, "Resource not found.", false}
	Conflict             = Code{"CONFLICT", http.StatusConflict, "State conflict.", false}
	Duplicate            = Code{"DUPLICATE", http.StatusConflict, "Resource already exists.", false}
	Validation           = Code{"VALIDATION_ERROR", http.StatusBadRequest, "Invalid request.", false}
	PayloadTooLarge      = Code{"PAYLOAD_TOO_LARGE", http.StatusRequestEntityTooLarge, "Payload too large.", false}
	UnsupportedMediaType = Code{"UNSUPPORTED_MEDIA_TYPE", http.StatusUnsupportedMediaType, "Unsupported media type.", false}
	ClientVersionTooOld  = Code{"CLIENT_VERSION_TOO_OLD", http.StatusUpgradeRequired, "Client version too old.", false}
	RateLimited          = Code{"RATE_LIMITED", http.StatusTooManyRequests, "Too many requests.", false}
	InternalError        = Code{"INTERNAL_ERROR", http.StatusInternalServerError, "Internal error.", true}
	UpstreamUnavailable  = Code{"UPSTREAM_UNAVAILABLE", http.StatusServiceUnavailable, "Upstream unavailable.", true}
	Gone                 = Code{"GONE", http.StatusGone, "Resource gone.", false}
)
