package inertia

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestHeadTags_ImportsGraph verifies the production tags walk the manifest's
// static-import graph the way Vite's backend integration does: CSS from the
// entry AND its imported chunks, modulepreload for every imported chunk (not
// the entry), deduped — and dynamic imports left alone.
func TestHeadTags_ImportsGraph(t *testing.T) {
	// entry → shared → vendor (static); entry also dynamically imports lazy.
	// shared is imported once but reachable via two paths to exercise dedup.
	const mf = `{
	  "src/main.ts": {"file":"assets/main.js","isEntry":true,"css":["assets/main.css"],
	                  "imports":["_shared.js","_vendor.js"],"dynamicImports":["_lazy.js"]},
	  "_shared.js":  {"file":"assets/shared.js","css":["assets/shared.css"],"imports":["_vendor.js"]},
	  "_vendor.js":  {"file":"assets/vendor.js"},
	  "_lazy.js":    {"file":"assets/lazy.js","css":["assets/lazy.css"]}
	}`
	fsys := fstest.MapFS{"dist/.vite/manifest.json": {Data: []byte(mf)}}
	m, err := loadManifest(fsys, "dist")
	if err != nil {
		t.Fatal(err)
	}
	tags := m.headTags()

	mustContain := func(want string) {
		t.Helper()
		if !strings.Contains(tags, want) {
			t.Errorf("headTags missing %q\ngot: %s", want, tags)
		}
	}
	mustNotContain := func(bad string) {
		t.Helper()
		if strings.Contains(tags, bad) {
			t.Errorf("headTags should not contain %q\ngot: %s", bad, tags)
		}
	}

	// Entry script.
	mustContain(`<script type="module" src="/assets/main.js"></script>`)
	// CSS from the entry and its imported chunks (recursive).
	mustContain(`<link rel="stylesheet" href="/assets/main.css">`)
	mustContain(`<link rel="stylesheet" href="/assets/shared.css">`)
	// modulepreload for statically-imported chunks.
	mustContain(`<link rel="modulepreload" href="/assets/shared.js">`)
	mustContain(`<link rel="modulepreload" href="/assets/vendor.js">`)
	// The entry itself is loaded via <script>, never modulepreloaded.
	mustNotContain(`<link rel="modulepreload" href="/assets/main.js">`)
	// Dynamic imports are loaded on demand: neither preloaded nor their CSS hoisted.
	mustNotContain(`/assets/lazy.js`)
	mustNotContain(`/assets/lazy.css`)

	// Dedup: vendor is reachable via entry and via shared, but appears once.
	if n := strings.Count(tags, `href="/assets/vendor.js"`); n != 1 {
		t.Errorf("vendor.js modulepreload should be deduped to 1, got %d\n%s", n, tags)
	}
}
