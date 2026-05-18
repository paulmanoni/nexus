//go:build cgo
// +build cgo

package vue

import "encoding/json"

// jsonUnmarshalString is the indirection point for the
// JSON-decoding step in compile.go's decodeResult. Wrapped so the
// import surface stays narrow at the top of compile.go (which is
// already heavy with QuickJS bindings). Tests that want to feed a
// pre-canned result through decodeResult without going through
// QuickJS use the same entry point.
func jsonUnmarshalString(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
