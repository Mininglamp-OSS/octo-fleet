package runtime

import (
	"github.com/Mininglamp-OSS/octo-fleet/internal/envelope"
	"github.com/Mininglamp-OSS/octo-fleet/internal/errcode"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// errresp.go — failure-side envelope rendering (spec R1/R2).
//
// fleet-local helper, mirroring the success-side resp.go: emits the
// { "error": {code, message, details, hint} } wire shape at the code's HTTP
// status. Stays self-contained (no octo-lib ErrorRenderer, no bump) until an
// official renderer ships.
//
// i18n is a future increment: renderError currently emits code.Message
// (English). A localized build swaps that single line for a lang-aware
// lookup (negotiate language from ctx / Accept-Language, resolve by
// code.Code) — call sites and wire shape don't change.

// responseError renders code with no details/hint.
func responseError(c *wkhttp.Context, code errcode.Code) {
	renderError(c, code, nil, "")
}

// responseErrorD renders code with structured details and an optional hint.
// For Internal (5xx) codes the details are dropped to avoid leaking cause —
// callers must have logged the underlying error already.
func responseErrorD(c *wkhttp.Context, code errcode.Code, details map[string]any, hint string) {
	renderError(c, code, details, hint)
}

func renderError(c *wkhttp.Context, code errcode.Code, details map[string]any, hint string) {
	if code.Internal {
		details = nil
		hint = ""
	}
	c.JSON(code.HTTPStatus, envelope.Error{Error: envelope.ErrorBody{
		Code:    code.Code,
		Message: code.Message, // i18n increment: localize by lang here
		Details: details,
		Hint:    hint,
	}})
}

// abortError renders the error envelope and Aborts the chain — for wkhttp
// middleware (auth gates) that must stop downstream handlers, unlike the
// responseError helpers which only write the body.
func abortError(c *wkhttp.Context, code errcode.Code) {
	c.AbortWithStatusJSON(code.HTTPStatus, envelope.Error{Error: envelope.ErrorBody{
		Code:    code.Code,
		Message: code.Message,
	}})
}
