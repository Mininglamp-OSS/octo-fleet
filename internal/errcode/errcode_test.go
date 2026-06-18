package errcode

import (
	"net/http"
	"testing"
)

// TestCodesHaveCanonicalStatus pins each enum to its spec HTTP status (R2),
// guarding against accidental status drift.
func TestCodesHaveCanonicalStatus(t *testing.T) {
	cases := []struct {
		c      Code
		code   string
		status int
	}{
		{AuthRequired, "AUTH_REQUIRED", http.StatusUnauthorized},
		{Forbidden, "FORBIDDEN", http.StatusForbidden},
		{NotFound, "NOT_FOUND", http.StatusNotFound},
		{Conflict, "CONFLICT", http.StatusConflict},
		{Duplicate, "DUPLICATE", http.StatusConflict},
		{Validation, "VALIDATION_ERROR", http.StatusBadRequest},
		{PayloadTooLarge, "PAYLOAD_TOO_LARGE", http.StatusRequestEntityTooLarge},
		{UnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", http.StatusUnsupportedMediaType},
		{ClientVersionTooOld, "CLIENT_VERSION_TOO_OLD", http.StatusUpgradeRequired},
		{RateLimited, "RATE_LIMITED", http.StatusTooManyRequests},
		{InternalError, "INTERNAL_ERROR", http.StatusInternalServerError},
		{UpstreamUnavailable, "UPSTREAM_UNAVAILABLE", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		if tc.c.Code != tc.code {
			t.Errorf("code = %q, want %q", tc.c.Code, tc.code)
		}
		if tc.c.HTTPStatus != tc.status {
			t.Errorf("%s status = %d, want %d", tc.code, tc.c.HTTPStatus, tc.status)
		}
		if tc.c.Message == "" {
			t.Errorf("%s has empty default message", tc.code)
		}
	}
}

// TestOnly5xxAreInternal asserts the Internal flag tracks 5xx exactly (4xx
// codes must surface their message; 5xx must hide cause).
func TestOnly5xxAreInternal(t *testing.T) {
	all := []Code{
		AuthRequired, Forbidden, NotFound, Conflict, Duplicate, Validation,
		PayloadTooLarge, UnsupportedMediaType, ClientVersionTooOld, RateLimited,
		InternalError, UpstreamUnavailable,
	}
	for _, c := range all {
		is5xx := c.HTTPStatus >= 500
		if c.Internal != is5xx {
			t.Errorf("%s Internal=%v but HTTPStatus=%d", c.Code, c.Internal, c.HTTPStatus)
		}
	}
}
