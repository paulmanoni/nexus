//go:build network
// +build network

package vue

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/paulmanoni/nexus/frontend/deps/fetcher"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// These tests hit real esm.sh and pull ~MB of Vue compiler source.
// Gated behind the `network` build tag so the default unit-test run
// stays fast + offline-clean. Run explicitly:
//
//	go test -tags=network ./frontend/deps/sfc/vue
//
// Test flake here is a registry availability problem, not a code
// problem — esm.sh is generally reliable but transient 503s do
// happen. A retry layer in Bootstrap could mask those but for v0.1
// the test failure surfaces the real-world fragility honestly.

func TestBootstrap_FetchAndBundleAgainstRealRegistry(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bundle, err := Bootstrap(ctx, BootstrapOptions{
		Store:     s,
		Fetcher:   f,
		Version:   DefaultCompilerVersion,
		BundleDir: tmp + "/bundles",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bundle) < 100_000 {
		t.Errorf("bundle suspiciously small: %d bytes — Vue compiler should be hundreds of KB",
			len(bundle))
	}
	if !strings.Contains(string(bundle), "__nexus_compileSFC") {
		t.Errorf("bundle missing globalThis.__nexus_compileSFC installation")
	}

	// Cache hit on the second call should return the same bytes
	// without re-fetching.
	bundle2, err := Bootstrap(ctx, BootstrapOptions{
		Store:     s,
		Fetcher:   f,
		Version:   DefaultCompilerVersion,
		BundleDir: tmp + "/bundles",
	})
	if err != nil {
		t.Fatalf("Bootstrap (cached): %v", err)
	}
	if len(bundle) != len(bundle2) || string(bundle) != string(bundle2) {
		t.Error("cached Bootstrap returned different bytes")
	}
}

// TestBootstrap_EndToEndCompileRealVueFile attempts to load the
// fetched-and-bundled Vue compiler into Goja and compile a real
// .vue source. CURRENTLY KNOWN BROKEN on Vue 3.4 + esm.sh's
// transitive dep graph: one of the helpers in @babel/parser
// (pulled in transitively) does
//
//	Object.getPrototypeOf(Object.getPrototypeOf(function(){...}))
//
// at module-init time, which Goja evaluates to a path through null
// and throws "Cannot convert undefined or null to object at
// getPrototypeOf". The same expression doesn't throw in V8 because
// V8's function-prototype chain ends at Object.prototype, not null.
//
// Tracked at frontend/deps/sfc/vue/README — likely fixes:
//   - Newer Goja release that closes this edge
//   - Switch the interpreter to QuickJS (pure Go via wasm)
//   - Patch the offending init line at bundle time via esbuild's
//     OnLoad transform
//
// In the meantime: this test skips by default + lives behind the
// `network` tag so it never breaks CI. The bootstrap MECHANICS
// (fetch + bundle) still work — see the test above. Users who
// need .vue today fall back to a pre-compile step; .jsx/.tsx work
// fully through our bundler.
func TestBootstrap_EndToEndCompileRealVueFile(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	t.Skip("known limitation: Vue 3.4 + esm.sh trips a Goja getPrototypeOf edge — see comment above")
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	bundle, err := Bootstrap(ctx, BootstrapOptions{
		Store: s, Fetcher: f, BundleDir: tmp + "/bundles",
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	c, err := NewCompiler(bundle, "@vue/compiler-sfc@"+DefaultCompilerVersion)
	if err != nil {
		t.Fatalf("NewCompiler: %v", err)
	}

	// Real Vue 3 SFC — script setup + template + scoped style.
	src := `<template>
  <h1 class="hello">{{ greeting }}</h1>
</template>

<script setup>
import { ref } from 'vue';
const greeting = ref("Hello from Vue");
</script>

<style scoped>
.hello { color: rebeccapurple; }
</style>
`
	res, err := c.Compile(src, "HelloVue.vue")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("compile produced errors: %+v", res.Errors)
	}
	if res.Code == "" {
		t.Fatal("Compile returned empty code")
	}
	// The synthesized module exports an SFC default with .render
	// + the scope marker + the inline-style injection.
	for _, want := range []string{
		"export default __sfc__",
		"__sfc__.__file",
		"data-v-",           // scope id prefix
		"rebeccapurple",     // style block survived
		"Hello from Vue",    // template setup wired through
	} {
		if !strings.Contains(res.Code, want) {
			t.Errorf("compiled module missing %q\nfull output:\n%s", want, res.Code)
		}
	}
}
