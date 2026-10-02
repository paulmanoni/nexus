// Package utils is the slice of templUI's utils the vendored components use.
// The components under internal/ui are copied from templUI
// (github.com/templui/templui, MIT — see ../LICENSE) and tuned to the
// dashboard's dense, square-cornered console style.
package utils

import twmerge "github.com/Oudwins/tailwind-merge-go"

// TwMerge combines Tailwind classes and resolves conflicts.
func TwMerge(classes ...string) string {
	return twmerge.Merge(classes...)
}

// If returns value if condition is true, otherwise the zero value of T.
func If[T any](condition bool, value T) T {
	var empty T
	if condition {
		return value
	}
	return empty
}
