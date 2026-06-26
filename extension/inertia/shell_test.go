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

	out := string(e.shell([]byte(`{"component":"X"}`), "n0nce", SSRResult{}))
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
	if got := string(e.shell([]byte(`{}`), "", SSRResult{})); strings.Contains(got, "nonce=") {
		t.Errorf("no nonce should mean no nonce attr; got: %s", got)
	}

	// A nonce with a quote can't break out of the attribute.
	if got := string(e.shell([]byte(`{}`), `a"b`, SSRResult{})); strings.Contains(got, `nonce="a"b"`) {
		t.Errorf("nonce must be attribute-escaped; got: %s", got)
	}
}

// TestShellSSR verifies SSR head tags are hoisted into <head> and the SSR body
// is placed inside the (data-server-rendered) root div for hydration; with no
// SSR result the root div stays empty and unflagged.
func TestShellSSR(t *testing.T) {
	e := &Engine{rootView: "app", head: `<script type="module" src="/app.js"></script>`}

	out := string(e.shell([]byte(`{"component":"X"}`), "",
		SSRResult{Head: []string{"<title>SSR</title>"}, Body: "<main>hi</main>"}))
	if !strings.Contains(out, "<title>SSR</title>") {
		t.Errorf("SSR head not hoisted: %s", out)
	}
	if !strings.Contains(out, `data-server-rendered="true"`) {
		t.Errorf("root must be flagged for hydration: %s", out)
	}
	if !strings.Contains(out, `<main>hi</main></div>`) {
		t.Errorf("SSR body must be inside the root div: %s", out)
	}

	// No SSR → empty, unflagged root div.
	plain := string(e.shell([]byte(`{}`), "", SSRResult{}))
	if strings.Contains(plain, "data-server-rendered") {
		t.Errorf("no SSR should leave the root unflagged: %s", plain)
	}
	if !strings.Contains(plain, `data-page="{}"></div>`) {
		t.Errorf("no SSR should leave the root empty: %s", plain)
	}
}
