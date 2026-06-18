package runtime

import (
	"net/http"
	"strconv"

	"github.com/Mininglamp-OSS/octo-fleet/internal/envelope"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
)

// resp.go — envelope response helpers, vendored from octo-lib PR #74
// (pkg/wkhttp/response_envelope.go), which is not yet merged.
//
// Adapted from Context methods to package-level functions: Go cannot add
// methods to the foreign *wkhttp.Context. When octo-lib ships these,
// delete this file and switch call sites to the official API —
// c.ResponseData(x) / c.ResponseCreated(x) / c.ResponseEmpty() (methods)
// and wkhttp.ResponseCursor(c, ...) / wkhttp.ResponseOffset(c, ...)
// (package-level functions, identical signatures to those here).
//
// Error responses keep going through RenderError / the injected
// ErrorRenderer (Stage 3), not these helpers.

// R5 pagination parameter bounds.
const (
	defaultPageSize = 20
	maxPageSize     = 100
	defaultPage     = 1
)

// ResponseData replies 200 with the single-object envelope: { "data": data } (R1).
func ResponseData(c *wkhttp.Context, data any) {
	c.JSON(http.StatusOK, envelope.Data[any]{Data: data})
}

// ResponseCreated replies 201 with the single-object envelope: { "data": data }
// (R1) — for create endpoints returning the new object.
func ResponseCreated(c *wkhttp.Context, data any) {
	c.JSON(http.StatusCreated, envelope.Data[any]{Data: data})
}

// ResponseEmpty replies 200 with the empty success envelope: { "data": {} }
// (R1) — for delete and state-machine actions. Distinct from the legacy
// ResponseOK ({"status":200}).
func ResponseEmpty(c *wkhttp.Context) {
	c.JSON(http.StatusOK, envelope.Data[envelope.EmptyResp]{})
}

// ResponseCursor replies 200 with the cursor-paginated list envelope
// (R1 + R5). A nil slice is normalized so the wire shows "data": [].
func ResponseCursor[T any](c *wkhttp.Context, items []T, hasMore bool, nextCursor string) {
	if items == nil {
		items = []T{}
	}
	c.JSON(http.StatusOK, envelope.CursorList[T]{
		Data:       items,
		Pagination: envelope.CursorPagination{HasMore: hasMore, NextCursor: nextCursor},
	})
}

// ResponseOffset replies 200 with the offset-paginated list envelope (R1 + R5).
func ResponseOffset[T any](c *wkhttp.Context, items []T, total int64, page, pageSize int) {
	if items == nil {
		items = []T{}
	}
	c.JSON(http.StatusOK, envelope.OffsetList[T]{
		Data:       items,
		Pagination: envelope.OffsetPagination{Total: total, Page: page, PageSize: pageSize},
	})
}

// GetCursorParams reads the R5 cursor-mode query params: `cursor` (opaque
// token, "" on first page) and `page_size` (default 20, capped at 100).
func GetCursorParams(c *wkhttp.Context) (cursor string, pageSize int) {
	return c.Query("cursor"), pageSizeParam(c)
}

// GetOffsetParams reads the R5 offset-mode query params: `page` (default 1)
// and `page_size` (default 20, capped at 100). Invalid values fall back.
func GetOffsetParams(c *wkhttp.Context) (page, pageSize int) {
	page, _ = strconv.Atoi(c.Query("page"))
	if page <= 0 {
		page = defaultPage
	}
	return page, pageSizeParam(c)
}

func pageSizeParam(c *wkhttp.Context) int {
	size, _ := strconv.Atoi(c.Query("page_size"))
	if size <= 0 {
		return defaultPageSize
	}
	if size > maxPageSize {
		return maxPageSize
	}
	return size
}
