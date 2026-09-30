package view

import "testing"

// The runtime is served as-is, so it must stay ASCII: a raw U+2028 or U+2029
// inside a regular expression literal ends the line in a browser.
func TestRuntimeIsASCII(t *testing.T) {
	for i, r := range runtimeJS {
		if r > 127 {
			t.Fatalf("runtime.js has %U at byte %d — write it as a \\u escape", r, i)
		}
	}
}
