package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnescapeTemplate(t *testing.T) {
	cases := map[string]string{
		`plain`:            `plain`,
		`a\\b`:             `a\b`,
		"esc tick: \\`":    "esc tick: `",
		`dollar \${x}`:     `dollar ${x}`,
		`mix \\ \` + "`" + ` \$`: `mix \ ` + "`" + ` $`,
	}
	for in, want := range cases {
		if got := unescapeTemplate(in); got != want {
			t.Errorf("unescapeTemplate(%q) = %q, want %q", in, got, want)
		}
	}
}

// sampleModule mimics what adapter.js assembles for an SFC with a
// scoped style, so the extractors can be tested without QuickJS.
func sampleModule(css string) string {
	return "const __sfc__ = {};\n" +
		"const __css = `" + css + "`;\n" +
		"if (typeof document !== 'undefined') {}\n" +
		"__sfc__.__file = \"F.vue\";\n" +
		"__sfc__.__scopeId = \"data-v-abc\";\n" +
		"export default __sfc__;\n"
}

func TestExtractCSSLiteral_Plain(t *testing.T) {
	css, interp := extractCSSLiteral(sampleModule(`.a[data-v-abc]{color:red}`))
	if interp {
		t.Errorf("plain css flagged as interpolated")
	}
	if css != `.a[data-v-abc]{color:red}` {
		t.Errorf("css = %q", css)
	}
}

func TestExtractCSSLiteral_Interpolated(t *testing.T) {
	_, interp := extractCSSLiteral(sampleModule("a{background:url(\"${__nl_url_0}\")}"))
	if !interp {
		t.Errorf("url() interpolation not detected — would be unsafe to hot-swap")
	}
}

func TestFingerprintVue(t *testing.T) {
	a := fingerprintVue(sampleModule(`.a{color:red}`))
	if a.id != "data-v-abc" {
		t.Errorf("id = %q, want data-v-abc", a.id)
	}
	if a.css != `.a{color:red}` {
		t.Errorf("css = %q", a.css)
	}
	// Same module except CSS differs → nonStyle identical, css differs.
	b := fingerprintVue(sampleModule(`.a{color:blue}`))
	if a.nonStyle != b.nonStyle {
		t.Errorf("nonStyle changed on a css-only edit: %q vs %q", a.nonStyle, b.nonStyle)
	}
	if a.css == b.css {
		t.Errorf("css fingerprint did not change on a css edit")
	}
}

func TestSwapInHMRClient(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "index.html")
	html := "<html><body><div id=app></div>\n  " + devReloadScriptTag + "\n</body></html>"
	if err := os.WriteFile(idx, []byte(html), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := swapInHMRClient(dir, "http://127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(idx)
	s := string(out)
	if strings.Contains(s, devReloadScriptTag) {
		t.Errorf("app reload tag should be replaced:\n%s", s)
	}
	if !strings.Contains(s, "http://127.0.0.1:9999/__nexus_hmr/client.js") {
		t.Errorf("hmr client tag missing:\n%s", s)
	}
	// Idempotent: a second swap shouldn't add a duplicate.
	if err := swapInHMRClient(dir, "http://127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	out2, _ := os.ReadFile(idx)
	if strings.Count(string(out2), "/__nexus_hmr/client.js") != 1 {
		t.Errorf("swap not idempotent:\n%s", out2)
	}
}

func TestSwapInHMRClient_NoAppTag_InjectsBeforeBody(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(dir, "index.html")
	if err := os.WriteFile(idx, []byte("<html><body>hi</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := swapInHMRClient(dir, "http://x"); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(idx)
	s := string(out)
	if !strings.Contains(s, "/__nexus_hmr/client.js") {
		t.Errorf("client not injected when no app tag present:\n%s", s)
	}
	if i, j := strings.Index(s, "client.js"), strings.Index(s, "</body>"); i < 0 || j < 0 || i > j {
		t.Errorf("client should sit before </body>:\n%s", s)
	}
}
func TestPreserveDevChunks_RestoresDeleted(t *testing.T) {
	out := t.TempDir()
	chunks := filepath.Join(out, "chunks")
	if err := os.MkdirAll(chunks, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build 1: route chunk A exists.
	a := filepath.Join(chunks, "RouteView-AAAA.js")
	if err := os.WriteFile(a, []byte("export default 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	preserveDevChunks(out) // archives A

	// Rebuild: esbuild deletes A's old hash, writes B.
	os.Remove(a)
	b := filepath.Join(chunks, "RouteView-BBBB.js")
	if err := os.WriteFile(b, []byte("export default 2"), 0o644); err != nil {
		t.Fatal(err)
	}
	preserveDevChunks(out) // should restore A and archive B

	// A must be back (a live page's stale import() resolves)...
	if _, err := os.Stat(a); err != nil {
		t.Errorf("deleted old-hash chunk was not restored: %v", err)
	}
	// ...and B must still be present (fresh loads get the new graph).
	if _, err := os.Stat(b); err != nil {
		t.Errorf("new chunk missing: %v", err)
	}
	// No leftover temp files in the live dir.
	if entries, _ := os.ReadDir(chunks); true {
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".tmp" {
				t.Errorf("leftover temp file: %s", e.Name())
			}
		}
	}
}

func TestPreserveDevChunks_NoChunksDir(t *testing.T) {
	// Single-entry build (no chunks/) must be a clean no-op, not a panic.
	preserveDevChunks(t.TempDir())
}
