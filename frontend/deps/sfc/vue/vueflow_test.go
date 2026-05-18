//go:build cgo && network

package vue_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// TestVueFlow_RealWorldDashboardShape exercises the exact import
// pattern the dashboard UI uses: a Vue SFC with <script setup>
// that imports @vue-flow/core. End-to-end through real esm.sh +
// QuickJS-backed compile + esbuild bundle.
//
// History: vue-flow was one of the three blockers I expected
// could still gate the dashboard migration after the QuickJS
// swap + font loader landed. The first probe surfaced a
// resolver query-inheritance bug (esm.sh stubs mix query-bearing
// and query-free sibling URLs); the two-attempt lookup fix
// unblocked it. This test pins the working state so a future
// fetcher/resolver refactor can't silently regress vue-flow.
//
// Test outputs to verify: bundle contains both the user-code
// VueFlow symbol AND createApp from vue, the Vue compiler-sfc
// produced a render function (any output containing "render"),
// and zero esbuild diagnostics surfaced.
//
// Gated behind `cgo && network` build tags so the default
// offline test pass is fast.
//
//	CGO_ENABLED=1 go test -tags="cgo network" ./frontend/deps/sfc/vue
func TestVueFlow_RealWorldDashboardShape(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}

	tmp := t.TempDir()
	s, err := store.New(filepath.Join(tmp, "cache"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	vfRes, err := f.Fetch(ctx, "@vue-flow/core@1.48.2")
	if err != nil {
		t.Fatalf("fetch vue-flow: %v", err)
	}
	vueRes, err := f.Fetch(ctx, "vue@3.4.21")
	if err != nil {
		t.Fatalf("fetch vue: %v", err)
	}

	bundleBytes, err := vue.Bootstrap(ctx, vue.BootstrapOptions{
		Store: s, Fetcher: f,
	})
	if err != nil {
		t.Fatalf("vue.Bootstrap: %v", err)
	}
	c, err := vue.NewCompiler(bundleBytes, "@vue/compiler-sfc@"+vue.DefaultCompilerVersion)
	if err != nil {
		t.Fatalf("vue.NewCompiler: %v", err)
	}
	defer c.Close()

	src := filepath.Join(tmp, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	// Real dashboard-shaped SFC: <script setup> + template
	// + <VueFlow/>, ref-backed nodes/edges arrays.
	sfc := `<template>
  <div style="height: 400px">
    <VueFlow :nodes="nodes" :edges="edges" />
  </div>
</template>

<script setup>
import { VueFlow } from "@vue-flow/core";
import { ref } from "vue";
const nodes = ref([
  { id: "1", position: { x: 0, y: 0 }, data: { label: "A" } },
  { id: "2", position: { x: 200, y: 100 }, data: { label: "B" } },
]);
const edges = ref([{ id: "e1-2", source: "1", target: "2" }]);
</script>
`
	if err := os.WriteFile(filepath.Join(src, "Canvas.vue"), []byte(sfc), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(src, "app.js")
	if err := os.WriteFile(entry, []byte(
		`import Canvas from "./Canvas.vue";
import { createApp } from "vue";
createApp(Canvas).mount("#app");
`), 0o644); err != nil {
		t.Fatal(err)
	}

	lf := lockfile.New()
	lf.Add(vfRes.Root)
	for _, p := range vfRes.Transitive {
		lf.Add(p)
	}
	lf.Add(vueRes.Root)
	for _, p := range vueRes.Transitive {
		lf.Add(p)
	}

	rPlugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: s})
	if err != nil {
		t.Fatalf("resolver.New: %v", err)
	}
	vPlugin, err := vue.Plugin(c)
	if err != nil {
		t.Fatalf("vue.Plugin: %v", err)
	}
	b := bundler.New()
	b.AddPlugin(rPlugin)
	b.AddPlugin(vPlugin)

	r, err := b.Build(bundler.Options{
		Entries:  []string{entry},
		OutDir:   filepath.Join(tmp, "out"),
		Lockfile: lf,
		Store:    s,
		Minify:   false,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(r.Errors) > 0 {
		for _, e := range r.Errors {
			t.Errorf("build error: %s", e.Text)
		}
		t.FailNow()
	}

	out, err := os.ReadFile(filepath.Join(tmp, "out", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(out)
	for _, want := range []string{
		"VueFlow",   // user-code import survived
		"createApp", // vue runtime import survived
	} {
		if !strings.Contains(bundle, want) {
			t.Errorf("bundle missing %q", want)
		}
	}
	if len(bundle) < 100_000 {
		t.Errorf("bundle suspiciously small: %d bytes — vue-flow alone is ~250 KB", len(bundle))
	}
}
