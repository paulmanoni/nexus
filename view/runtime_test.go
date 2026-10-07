package view

import "testing"

// The runtime is served as-is, so it must stay ASCII: a raw U+2028 or U+2029
// inside a regular expression literal ends the line in a browser.
func TestRuntimeIsASCII(t *testing.T) {
	for name, src := range map[string]string{"runtime.js": runtimeJS, "behaviors.js": behaviorsJS} {
		for i, r := range src {
			if r > 127 {
				t.Fatalf("%s has %U at byte %d — write it as a \\u escape", name, r, i)
			}
		}
	}
}
