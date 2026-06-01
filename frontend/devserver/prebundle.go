package devserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// shortHash returns a short stable hex digest of s, used to disambiguate
// pre-bundle URLs for different entry sub-paths of the same package.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// Dependency pre-bundling — the unbundled dev server's answer to the
// native-ESM waterfall. Without it the browser fetches one HTTP request per
// dependency module; a Vuetify app fans out to ~1800 intra-package modules,
// which makes the first cold load crawl. Vite solves this with optimizeDeps
// (esbuild-bundle each npm dep into one file up front). This is the same
// idea, on demand:
//
//   - The FIRST time a dependency entry URL is requested, esbuild bundles it
//     — INLINING every sibling module in the SAME package (read from the
//     store) so the package's whole intra-fan-out collapses into one file.
//   - Cross-package imports (e.g. vuetify importing `vue`) are kept EXTERNAL
//     and rewritten to the existing /@dep/ per-module URL. That URL is
//     byte-identical to what the non-prebundled path serves, so a dependency
//     like vue still resolves to ONE shared module instance — the invariant
//     that makes state-preserving HMR work. Pre-bundling never crosses a
//     package boundary, so it can't fork an instance.
//   - Non-JS imports (CSS / fonts / images) inside a package are also kept
//     external (served as their usual CSS/asset modules), since this is a
//     JS bundle with no output path.
//
// Fail-safe: a package that fails to bundle returns an error and the caller
// falls back to serving that dependency per-module. Pre-bundling is a pure
// performance optimisation layered over the working per-module path.

// PrebundlePrefix is the URL namespace pre-bundled package entries are
// served under (distinct from DepPrefix's per-module blobs).
const PrebundlePrefix = "/@pre/"

// prebundler bundles dependency package entries on demand and caches the
// results. Safe for concurrent use.
type prebundler struct {
	host *Host

	mu    sync.Mutex
	cache map[string]prebundleResult // entry storeURL → result
}

type prebundleResult struct {
	js  string
	err error
}

func newPrebundler(h *Host) *prebundler {
	return &prebundler{host: h, cache: map[string]prebundleResult{}}
}

// pkgKeyRE pulls "pkg@ver" (incl. scoped) out of an esm.sh URL:
//
//	https://esm.sh/vue@3.5.34/es2022/vue.mjs                  → vue@3.5.34
//	https://esm.sh/@vue/runtime-core@3.5.34/es2022/x.mjs      → @vue/runtime-core@3.5.34
//	https://esm.sh/vuetify@3.11.7/X-ZX.../es2022/components.mjs → vuetify@3.11.7
//
// Version segment stops at the first /, ?, or & so a query string
// (?external=vue,react-dom) or sub-path never leaks into the package key.
var pkgKeyRE = regexp.MustCompile(`esm\.sh/((?:@[^/]+/)?[^/@?&]+@[^/?&]+)`)

// pkgKey returns the "name@version" identity of a registry URL, or "" when
// the URL isn't a recognizable esm.sh package URL.
func pkgKey(url string) string {
	m := pkgKeyRE.FindStringSubmatch(url)
	if m == nil {
		return ""
	}
	return m[1]
}

// bundle returns the pre-bundled JS for a package entry identified by its
// canonical store URL. Cached after the first build (including failures, so
// a doomed package isn't retried every request). vue itself is never
// pre-bundled — it's the shared-instance anchor and is tiny enough served
// per-module; pre-bundling it would only add risk.
func (p *prebundler) bundle(entryStoreURL string) (string, error) {
	p.mu.Lock()
	if r, ok := p.cache[entryStoreURL]; ok {
		p.mu.Unlock()
		return r.js, r.err
	}
	p.mu.Unlock()

	js, err := p.build(entryStoreURL)
	p.mu.Lock()
	p.cache[entryStoreURL] = prebundleResult{js: js, err: err}
	p.mu.Unlock()
	return js, err
}

// build runs the esbuild sub-build for one package entry.
func (p *prebundler) build(entryStoreURL string) (string, error) {
	entryPkg := pkgKey(entryStoreURL)
	if entryPkg == "" {
		return "", fmt.Errorf("prebundle: not a package URL: %s", entryStoreURL)
	}

	// Entry contents = the entry blob itself (preserves its exact default +
	// named exports). Its imports resolve through the plugin below.
	entryBlob, _, ok := p.host.loadDepBytes(entryStoreURL)
	if !ok {
		return "", fmt.Errorf("prebundle: entry blob not in cache: %s", entryStoreURL)
	}

	const ns = "nexus-prebundle"
	plugin := api.Plugin{
		Name: "nexus-prebundle",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				// Base URL the import resolves against: the importer's own
				// registry URL (in our namespace) or the entry URL (stdin).
				base := entryStoreURL
				if args.Namespace == ns && args.Importer != "" {
					base = args.Importer
				}
				storeURL, found, _ := p.host.res.ResolveURL(args.Path, base)
				if !found || storeURL == "" {
					// Can't resolve through the registry — leave it for
					// esbuild's default (will usually error, surfacing a
					// clear miss). Don't claim it.
					return api.OnResolveResult{}, nil
				}
				// Same package + JS → inline (read from store in OnLoad).
				if pkgKey(storeURL) == entryPkg && p.host.depURLIsJS(storeURL) {
					return api.OnResolveResult{Path: storeURL, Namespace: ns}, nil
				}
				// Cross-package, or non-JS (CSS/asset): keep EXTERNAL and
				// point at the per-module /@dep/ URL. Cross-package externals
				// preserve single-instance sharing; non-JS is served as the
				// usual CSS/asset module.
				return api.OnResolveResult{Path: p.host.toDepPath(storeURL), External: true}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: ns}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				body, kind, ok := p.host.loadDepBytes(args.Path)
				if !ok {
					return api.OnLoadResult{}, fmt.Errorf("prebundle: blob not in cache: %s", args.Path)
				}
				loader := api.LoaderJS
				if kind == "css" {
					loader = api.LoaderCSS // shouldn't happen (CSS externalized), defensive
				}
				s := string(body)
				return api.OnLoadResult{Contents: &s, Loader: loader}, nil
			})
		},
	}

	entryStr := string(entryBlob)
	result := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   entryStr,
			Sourcefile: pkgKey(entryStoreURL) + ".js",
			Loader:     api.LoaderJS,
		},
		Bundle:   true,
		Write:    false,
		Format:   api.FormatESModule,
		Target:   api.ES2022,
		Platform: api.PlatformBrowser,
		Plugins:  []api.Plugin{plugin},
		LogLevel: api.LogLevelSilent,
		// Vue's esm-bundler flags as Defines — same as the main transform —
		// so any flag references inside a pre-bundled dep are satisfied.
		Define: p.host.defines,
	})
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("prebundle %s: %s", entryPkg, result.Errors[0].Text)
	}
	if len(result.OutputFiles) == 0 {
		return "", fmt.Errorf("prebundle %s: no output", entryPkg)
	}
	return string(result.OutputFiles[0].Contents), nil
}

// prebundleEligible reports whether a resolved dependency store URL should be
// pre-bundled rather than served per-module. Only JS package modules are
// eligible. The ENTIRE Vue family (vue + @vue/*) is excluded and kept
// per-module: those packages carry the single-instance invariant (one
// reactivity system, one runtime-core that installs __VUE_HMR_RUNTIME__,
// one set of vnode shapes). Pre-bundling them risks inlining a second copy
// of runtime-core/reactivity into a sibling bundle and forking the
// instance, which would silently break reactivity + HMR. Everything else
// (vuetify, pinia, apollo, …) is safe to bundle — it imports the Vue family
// by URL, so it shares the one per-module instance.
func prebundleEligible(storeURL string) bool {
	pk := pkgKey(storeURL)
	if pk == "" {
		return false
	}
	if pk == "vue" || strings.HasPrefix(pk, "vue@") || strings.HasPrefix(pk, "@vue/") {
		return false
	}
	return true
}

// --- Host helpers shared with the per-module path -----------------------

// loadDepBytes reads a cached dependency blob by its canonical store URL,
// returning the bytes + module kind ("js"/"css"/"asset"). Mirrors loadDep's
// store read (incl. the on-demand cold-miss fetch) without the URL encoding
// or CSS url() rewriting — the prebundler works in store-URL space.
func (h *Host) loadDepBytes(storeURL string) ([]byte, string, bool) {
	blob, meta, err := h.res.Store.Get(storeURL)
	if err != nil {
		if h.res.FetchOnDemand != nil {
			if c, ferr := h.res.FetchOnDemand(storeURL); ferr == nil && c != "" {
				blob, meta, err = h.res.Store.Get(c)
			}
		}
		if err != nil {
			return nil, "", false
		}
	}
	body, rerr := os.ReadFile(blob)
	if rerr != nil {
		return nil, "", false
	}
	return body, kindForContentType(meta.ContentType, storeURL), true
}

// depURLIsJS reports whether a dependency store URL is a JS module (vs CSS or
// an asset), used to decide what the prebundler may inline.
func (h *Host) depURLIsJS(storeURL string) bool {
	_, meta, err := h.res.Store.Get(storeURL)
	if err != nil {
		// Unknown → infer from extension; default to JS (esm.sh's norm).
		return kindForContentType("", storeURL) == "js"
	}
	return kindForContentType(meta.ContentType, storeURL) == "js"
}
