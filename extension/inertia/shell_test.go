package inertia

import (
	"strings"
	"testing"
)

// TestShellNonce verifies the document shell stamps a per-request CSP nonce on
// every engine-injected <script>/<link> (asset tags + Config.Head), and emits
// none when no nonce is supplied.
func TestShellNonce(t *testing.T) {
	e := &Engine{
		rootView:   "app",
		customHead: `<link rel="stylesheet" href="/app.css">`,
		head:       `<link rel="modulepreload" href="/dep.js"><script type="module" src="/app.js"></script>`,
	}

	out := string(e.shell([]byte(`{"component":"X"}`), "n0nce"))
	for _, want := range []string{
		`<script nonce="n0nce" type="module" src="/app.js">`,
		`<link nonce="n0nce" rel="modulepreload" href="/dep.js">`,
		`<link nonce="n0nce" rel="stylesheet" href="/app.css">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nonce not stamped; missing %q\ngot: %s", want, out)
		}
	}

	// No nonce → no nonce attribute anywhere.
	if got := string(e.shell([]byte(`{}`), "")); strings.Contains(got, "nonce=") {
		t.Errorf("no nonce should mean no nonce attr; got: %s", got)
	}

	// A nonce with a quote can't break out of the attribute.
	if got := string(e.shell([]byte(`{}`), `a"b`)); strings.Contains(got, `nonce="a"b"`) {
		t.Errorf("nonce must be attribute-escaped; got: %s", got)
	}
}
