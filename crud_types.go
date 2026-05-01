package nexus

import "errors"

// ListOptions is the parsed shape of a List request's query string.
// AsCRUD's generated List handler reads ?limit / ?offset / ?sort and
// hands a populated ListOptions to the Store.
type ListOptions struct {
	// Limit caps the page size. AsCRUD clamps to [1, 100] (default 20)
	// before invoking the Store, so user code can trust the bounds.
	Limit int `json:"limit" query:"limit"`
	// Offset is the start index, default 0. Negative values are
	// clamped to 0 by the framework.
	Offset int `json:"offset" query:"offset"`
	// Sort is a CSV of field names; "-" prefix indicates descending.
	// Stores interpret these against their own column names.
	Sort []string `json:"sort,omitempty" query:"sort"`
}

// Page is the standard list-response envelope. Generic so callers
// (tests, generated SDKs) keep type safety on the items slice.
type Page[T any] struct {
	Items  []T `json:"items"`
	Total  int `json:"total"`
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Sentinel errors that AsCRUD maps to HTTP status codes:
//
//	ErrCRUDNotFound   → 404
//	ErrCRUDConflict   → 409
//	ErrCRUDValidation → 400
//
// Stores wrap or return these so transport-level mapping stays in
// one place. Anything else maps to 500.
var (
	ErrCRUDNotFound   = errors.New("crud: not found")
	ErrCRUDConflict   = errors.New("crud: conflict")
	ErrCRUDValidation = errors.New("crud: validation error")
)