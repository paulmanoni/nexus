//go:build cgo && network

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
// Gated behind both the `cgo` AND `network` build tags so the
// default unit-test run stays fast + offline-clean. Run with:
//
//	CGO_ENABLED=1 go test -tags="cgo network" ./frontend/deps/sfc/vue
//
// Test flake here is registry availability, not code — esm.sh is
// generally reliable but transient 503s happen.

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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
		t.Errorf("bundle suspiciously small: %d bytes — Vue compiler should be hundreds of KB", len(bundle))
	}
	if !strings.Contains(string(bundle), "__nexus_compileSFC") {
		t.Errorf("bundle missing globalThis.__nexus_compileSFC installation")
	}

	// Cache hit on second call returns identical bytes without
	// re-fetching.
	bundle2, err := Bootstrap(ctx, BootstrapOptions{
		Store:     s,
		Fetcher:   f,
		Version:   DefaultCompilerVersion,
		BundleDir: tmp + "/bundles",
	})
	if err != nil {
		t.Fatalf("Bootstrap (cached): %v", err)
	}
	if string(bundle) != string(bundle2) {
		t.Error("cached Bootstrap returned different bytes")
	}
}

// TestBootstrap_EndToEndCompileRealVueFile is the headline proof
// the QuickJS swap actually works against production Vue. Loads
// the real @vue/compiler-sfc into QuickJS, compiles a real .vue
// source with script-setup + scoped style, asserts the output
// has the expected shape.
//
// This is THE test that gated the Goja-based path: under Goja, the
// bundle threw at module-eval time on the @babel/parser async-
// generator prototype-chain introspection. Under QuickJS, that
// expression evaluates correctly because async generators are
// native — so this test should pass cleanly.
func TestBootstrap_EndToEndCompileRealVueFile(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	defer c.Close()

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
	for _, want := range []string{
		"export default __sfc__",
		"__sfc__.__file",
		"data-v-",        // scope id prefix
		"rebeccapurple",  // style block survived
		"Hello from Vue", // template setup wired through
	} {
		if !strings.Contains(res.Code, want) {
			t.Errorf("compiled module missing %q\nfull output:\n%s", want, res.Code)
		}
	}
}

// TestBootstrap_StyleUrlsHoistedToImports proves CSS url() refs in a
// scoped style are rewritten into bundler-resolvable ESM imports
// (Vite-equivalent) rather than shipped as literal strings. An
// aliased asset becomes `import __nl_url_N from "@/..."` + a
// template-literal interpolation, while external/data/root-absolute
// URLs pass through untouched.
func TestBootstrap_StyleUrlsHoistedToImports(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	defer c.Close()

	src := `<template><div class="bg"></div></template>
<style scoped>
.bg {
  background-image: url('@/assets/images/flag.png');
  cursor: url("./cur.png?v=2"), pointer;
  mask: url(https://cdn.example.com/m.svg);
  list-style: url(/root.svg);
}
</style>
`
	res, err := c.Compile(src, "Bg.vue")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("compile produced errors: %+v", res.Errors)
	}

	for _, want := range []string{
		`import __nl_url_0 from "@/assets/images/flag.png";`, // aliased → import (clean spec)
		`import __nl_url_1 from "./cur.png";`,                // relative → import, ?query stripped from spec
		"url(\"${__nl_url_0}\")",                             // interpolated back into CSS
		"${__nl_url_1}?v=2",                                  // query suffix re-appended after resolution
		"https://cdn.example.com/m.svg",                     // external URL untouched
		"url(/root.svg)",                                     // root-absolute untouched
	} {
		if !strings.Contains(res.Code, want) {
			t.Errorf("compiled module missing %q\nfull output:\n%s", want, res.Code)
		}
	}
	// External + root-absolute must NOT be hoisted to imports.
	if strings.Contains(res.Code, "import __nl_url_2") {
		t.Errorf("external/absolute url() should not be hoisted to an import\nfull output:\n%s", res.Code)
	}
}

// TestBootstrap_UnsupportedFeaturesRejected proves the adapter fails
// loudly (clear error, empty code) for SFC features the synchronous
// path can't honor, instead of emitting silently-wrong output.
func TestBootstrap_UnsupportedFeaturesRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	defer c.Close()

	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{
			name:    "css modules",
			src:     "<template><p>x</p></template>\n<style module>.a{color:red}</style>",
			wantSub: "<style module> is not supported",
		},
		{
			name:    "style preprocessor",
			src:     "<template><p>x</p></template>\n<style lang=\"scss\">.a{.b{color:red}}</style>",
			wantSub: `<style lang="scss"> requires a preprocessor`,
		},
		{
			name:    "template preprocessor",
			src:     "<template lang=\"pug\">p hi</template>\n<style>.a{color:red}</style>",
			wantSub: `<template lang="pug"> requires a preprocessor`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Compile(tc.src, "Bad.vue")
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if res.Code != "" {
				t.Errorf("expected empty code on rejection, got:\n%s", res.Code)
			}
			if len(res.Errors) == 0 {
				t.Fatal("expected a guard error, got none")
			}
			var found bool
			for _, e := range res.Errors {
				if strings.Contains(e.Message, tc.wantSub) {
					found = true
				}
			}
			if !found {
				t.Errorf("no error contained %q; got %+v", tc.wantSub, res.Errors)
			}
		})
	}
}

// TestBootstrap_ScopeIdFoldsSource proves the scope id depends on
// source content, not just the filename — two distinct sources
// compiled under the SAME filename must get different scope ids so
// their scoped CSS can't cross-contaminate.
func TestBootstrap_ScopeIdFoldsSource(t *testing.T) {
	if testing.Short() {
		t.Skip("network test")
	}
	tmp := t.TempDir()
	s, err := store.New(tmp + "/cache")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	f := fetcher.New(s, "")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
	defer c.Close()

	extractScope := func(code string) string {
		const marker = "__sfc__.__scopeId = "
		i := strings.Index(code, marker)
		if i == -1 {
			return ""
		}
		rest := code[i+len(marker):]
		// value is a JSON string literal like "data-v-abc123"
		start := strings.IndexByte(rest, '"')
		if start == -1 {
			return ""
		}
		end := strings.IndexByte(rest[start+1:], '"')
		if end == -1 {
			return ""
		}
		return rest[start+1 : start+1+end]
	}

	a, err := c.Compile("<template><p>A</p></template>\n<style scoped>.a{color:red}</style>", "Same.vue")
	if err != nil {
		t.Fatalf("Compile A: %v", err)
	}
	b, err := c.Compile("<template><p>B</p></template>\n<style scoped>.b{color:blue}</style>", "Same.vue")
	if err != nil {
		t.Fatalf("Compile B: %v", err)
	}
	sa, sb := extractScope(a.Code), extractScope(b.Code)
	if sa == "" || sb == "" {
		t.Fatalf("could not extract scope ids: a=%q b=%q", sa, sb)
	}
	if sa == sb {
		t.Errorf("same filename + different source produced identical scope id %q — source not folded into hash", sa)
	}
}
