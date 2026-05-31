package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// Stage 2c-2: per-edit Vue HMR update modules.
//
// When a .vue's template or script changes (not just CSS), we compile
// JUST that file into a standalone ES module and serve it over the HMR
// loopback server. The browser import()s it and hands the fresh
// render/component to Vue's HMR runtime (rerender/reload), so the
// component updates in place WITHOUT a full reload — preserving app
// state.
//
// The one hard constraint: the update module's Vue must be the SAME
// instance as the running app's (Vue HMR requires single-instance vnode
// identity). We can't re-bundle Vue into the update module. Instead the
// update module's `import ... from "vue"` is rewritten to read the
// instance the app pinned on globalThis.__nexus_vue__ at boot (the
// Stage 2c-1 bridge). A virtual `vue` module supplies that.
//
// Lifecycle: classifyChange already returns the compiled SFC JS + the
// hmrId. buildVueUpdateModule turns that into a browser-ready ESM string
// (Vue externalized to the global). The hmrServer caches it by URL and
// serves it; the SSE message carries the URL.

// buildVueUpdateModule compiles one .vue's already-compiled SFC JS into a
// standalone ESM whose `vue` import binds to globalThis.__nexus_vue__.
// Returns the module bytes ready to serve. compiledSFC is the output of
// the SFC compiler (CompileResult.Code) for the changed file.
func buildVueUpdateModule(compiledSFC, filename string) (string, error) {
	// Names the compiled SFC imports from "vue" — esbuild needs the
	// virtual vue module to export exactly these as live bindings.
	names := scanVueImports(compiledSFC)
	var shim strings.Builder
	shim.WriteString("const __v = (globalThis.__nexus_vue__ || {});\n")
	shim.WriteString("export default (__v.default || __v);\n")
	for _, n := range names {
		if n == "default" {
			continue
		}
		fmt.Fprintf(&shim, "export const %s = __v[%q];\n", n, n)
	}
	vueShim := shim.String()

	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   compiledSFC,
			ResolveDir: filepath.Dir(filename),
			Sourcefile: filepath.Base(filename),
			Loader:     api.LoaderTS, // SFC output carries TS annotations
		},
		Bundle:   true,
		Write:    false,
		Format:   api.FormatESModule,
		Platform: api.PlatformBrowser,
		Plugins:  []api.Plugin{vueVirtualPlugin(vueShim)},
		LogLevel: api.LogLevelSilent,
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("hmr update build: %s", result.Errors[0].Text)
	}
	if len(result.OutputFiles) == 0 {
		return "", fmt.Errorf("hmr update build: no output")
	}
	return string(result.OutputFiles[0].Contents), nil
}

// vueVirtualPlugin makes any `import ... from "vue"` resolve to an
// in-memory module whose body is shimJS (re-exports globalThis
// .__nexus_vue__). Keeps the update module from bundling a 2nd Vue.
func vueVirtualPlugin(shimJS string) api.Plugin {
	return api.Plugin{
		Name: "nexus-vue-virtual",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: `^vue$`}, func(api.OnResolveArgs) (api.OnResolveResult, error) {
				return api.OnResolveResult{Path: "vue", Namespace: "nexus-vue-virtual"}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: `.*`, Namespace: "nexus-vue-virtual"}, func(api.OnLoadArgs) (api.OnLoadResult, error) {
				c := shimJS
				loader := api.LoaderJS
				return api.OnLoadResult{Contents: &c, Loader: loader}, nil
			})
		},
	}
}

// scanVueImports extracts the named identifiers a compiled SFC imports
// from "vue". The SFC compiler emits one of:
//
//	import { openBlock as _openBlock, createElementBlock as _ceb } from "vue"
//	import { defineComponent } from "vue"
//
// We return the ORIGINAL names (openBlock, createElementBlock), which is
// what the virtual module must export. esbuild rewrites the local
// aliases against those exports.
func scanVueImports(code string) []string {
	seen := map[string]bool{}
	var out []string
	for _, stmt := range vueImportStatements(code) {
		inner := stmt
		if i := strings.IndexByte(inner, '{'); i >= 0 {
			inner = inner[i+1:]
		}
		if i := strings.IndexByte(inner, '}'); i >= 0 {
			inner = inner[:i]
		}
		for _, part := range strings.Split(inner, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			// "openBlock as _openBlock" → "openBlock"
			if sp := strings.Fields(part); len(sp) > 0 {
				name := sp[0]
				if name != "" && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
	}
	return out
}

// vueImportStatements returns the bodies of every `import {...} from
// "vue"` statement in code. Cheap string scan — adequate for the SFC
// compiler's deterministic output (no minification at this stage).
func vueImportStatements(code string) []string {
	var stmts []string
	const fromVue = `from "vue"`
	rest := code
	for {
		idx := strings.Index(rest, fromVue)
		if idx < 0 {
			break
		}
		// Walk back to the `import` keyword that owns this `from`.
		head := rest[:idx]
		ii := strings.LastIndex(head, "import")
		if ii >= 0 {
			stmts = append(stmts, head[ii:])
		}
		rest = rest[idx+len(fromVue):]
	}
	// Also handle single-quoted form: from 'vue'
	rest = code
	const fromVueSingle = `from 'vue'`
	for {
		idx := strings.Index(rest, fromVueSingle)
		if idx < 0 {
			break
		}
		head := rest[:idx]
		ii := strings.LastIndex(head, "import")
		if ii >= 0 {
			stmts = append(stmts, head[ii:])
		}
		rest = rest[idx+len(fromVueSingle):]
	}
	return stmts
}

// updateModuleCache stores the most recent built update module per URL so
// the HTTP handler can serve it. Bounded by one entry per hmrId; old
// generations are overwritten (a stale URL just 404s, which the client
// treats as "fall back to reload").
type updateModuleCache struct {
	mu      sync.Mutex
	modules map[string]string // url path → module JS
}

func newUpdateModuleCache() *updateModuleCache {
	return &updateModuleCache{modules: map[string]string{}}
}

func (c *updateModuleCache) put(urlPath, js string) {
	c.mu.Lock()
	c.modules[urlPath] = js
	c.mu.Unlock()
}

func (c *updateModuleCache) get(urlPath string) (string, bool) {
	c.mu.Lock()
	js, ok := c.modules[urlPath]
	c.mu.Unlock()
	return js, ok
}
