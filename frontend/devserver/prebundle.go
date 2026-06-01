package devserver

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
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

// prebundler bundles dependency packages on demand and caches the results.
// Safe for concurrent use.
//
// Dedup model: a package's sub-path entries (vuetify, vuetify/components,
// vuetify/directives, …) are NOT bundled independently — that inlined a
// fresh copy of every shared internal module into each entry. Instead all
// known entries of a package are built TOGETHER in one esbuild build with
// code-splitting, so shared internals land in a common chunk both entries
// import. Entries are registered (by ResolveImport via toPrebundle) as the
// browser walks the graph; the build runs lazily on first fetch and is
// rebuilt only if the entry set grew.
type prebundler struct {
	host *Host

	mu      sync.Mutex
	entries map[string]map[string]bool // pkgKey → set of entry store URLs
	built   map[string]*pkgBuild       // pkgKey → last build (nil until built)
}

// pkgBuild is one package's grouped, code-split build: a map of served
// basename → file contents (entries + shared chunks), plus the entry-set
// signature it was built from (to detect when a rebuild is needed) and any
// build error.
type pkgBuild struct {
	files map[string]string // basename (e-<hash>.js / chunk-<hash>.js) → JS
	sig   string            // sorted entry-set fingerprint
	err   error
}

func newPrebundler(h *Host) *prebundler {
	return &prebundler{
		host:    h,
		entries: map[string]map[string]bool{},
		built:   map[string]*pkgBuild{},
	}
}

// register records an entry store URL under its package so the next build
// of that package covers it. Returns the package key + the entry's stable
// served basename (e-<hash>.js).
func (p *prebundler) register(entryStoreURL string) (pkg, base string) {
	pkg = pkgKey(entryStoreURL)
	base = "e-" + shortHash(entryStoreURL) + ".js"
	p.mu.Lock()
	if p.entries[pkg] == nil {
		p.entries[pkg] = map[string]bool{}
	}
	p.entries[pkg][entryStoreURL] = true
	p.mu.Unlock()
	return pkg, base
}

// serve returns the JS for a served prebundle basename within a package,
// building (or rebuilding) the package's grouped split bundle as needed.
func (p *prebundler) serve(pkg, base string) (string, error) {
	p.mu.Lock()
	want := p.entrySig(pkg)
	b := p.built[pkg]
	if b == nil || b.sig != want {
		entries := make([]string, 0, len(p.entries[pkg]))
		for u := range p.entries[pkg] {
			entries = append(entries, u)
		}
		p.mu.Unlock()
		nb := p.buildPackage(pkg, entries, want) // network/CPU outside the lock
		p.mu.Lock()
		// Keep the freshest build (another goroutine may have raced).
		if cur := p.built[pkg]; cur == nil || cur.sig != want {
			p.built[pkg] = nb
		}
		b = p.built[pkg]
	}
	p.mu.Unlock()

	if b.err != nil {
		return "", b.err
	}
	if js, ok := b.files[base]; ok {
		return js, nil
	}
	return "", fmt.Errorf("prebundle: %s not in package build %s", base, pkg)
}

// entrySig fingerprints a package's current entry set (caller holds mu).
func (p *prebundler) entrySig(pkg string) string {
	es := make([]string, 0, len(p.entries[pkg]))
	for u := range p.entries[pkg] {
		es = append(es, u)
	}
	sort.Strings(es)
	return strings.Join(es, "\n")
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

// buildPackage runs ONE esbuild build over ALL of a package's known entry
// store URLs with code-splitting on, so modules shared between sub-path
// entries (vuetify vs vuetify/components) land in a common chunk instead of
// being inlined into each entry. Returns a pkgBuild mapping served basename
// → JS (one e-<hash>.js per entry + chunk-*.js shared chunks). sig is the
// entry-set fingerprint the build was made from.
//
// Resolution rules (unchanged from the per-entry build): same-package JS is
// inlined/split internally; cross-package and non-JS imports stay EXTERNAL
// pointed at /@dep/ URLs, preserving the single shared instance for vue and
// serving CSS/assets as their usual modules.
func (p *prebundler) buildPackage(entryPkg string, entryURLs []string, sig string) *pkgBuild {
	if entryPkg == "" || len(entryURLs) == 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle: no entries for %q", entryPkg)}
	}

	const ns = "nexus-prebundle"
	// Map each entry's synthetic input path → its store URL. OutputPath
	// e-<hash> matches the served basename register() hands out.
	entryURLByInput := map[string]string{}
	var eps []api.EntryPoint
	for _, u := range entryURLs {
		input := "nexus-entry:" + u // synthetic; claimed in OnResolve below
		entryURLByInput[input] = u
		eps = append(eps, api.EntryPoint{InputPath: input, OutputPath: "e-" + shortHash(u)})
	}

	plugin := api.Plugin{
		Name: "nexus-prebundle",
		Setup: func(b api.PluginBuild) {
			b.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				// Entry points arrive as our synthetic "nexus-entry:" inputs.
				if storeURL, ok := entryURLByInput[args.Path]; ok {
					return api.OnResolveResult{Path: storeURL, Namespace: ns}, nil
				}
				// Otherwise resolve against the importer's registry URL.
				base := ""
				if args.Namespace == ns && args.Importer != "" {
					base = args.Importer
				}
				storeURL, found, _ := p.host.res.ResolveURL(args.Path, base)
				if !found || storeURL == "" {
					return api.OnResolveResult{}, nil // leave to esbuild default
				}
				// Same package + JS → keep internal (inlined or split into a
				// shared chunk by esbuild).
				if pkgKey(storeURL) == entryPkg && p.host.depURLIsJS(storeURL) {
					return api.OnResolveResult{Path: storeURL, Namespace: ns}, nil
				}
				// Cross-package or non-JS → EXTERNAL /@dep/ URL (shared
				// instance for vue; CSS/asset served as usual).
				return api.OnResolveResult{Path: p.host.toDepPath(storeURL), External: true}, nil
			})
			b.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: ns}, func(args api.OnLoadArgs) (api.OnLoadResult, error) {
				body, _, ok := p.host.loadDepBytes(args.Path)
				if !ok {
					return api.OnLoadResult{}, fmt.Errorf("prebundle: blob not in cache: %s", args.Path)
				}
				s := string(body)
				return api.OnLoadResult{Contents: &s, Loader: api.LoaderJS}, nil
			})
		},
	}

	result := api.Build(api.BuildOptions{
		EntryPointsAdvanced: eps,
		Bundle:              true,
		Write:               false,
		Splitting:           true,             // shared internals → common chunk
		Outdir:              "/nexus-pre-out", // virtual; Write:false
		ChunkNames:          "chunk-[hash]",
		Format:              api.FormatESModule,
		Target:              api.ES2022,
		Platform:            api.PlatformBrowser,
		Plugins:             []api.Plugin{plugin},
		LogLevel:            api.LogLevelSilent,
		Define:              p.host.defines,
	})
	if len(result.Errors) > 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle %s: %s", entryPkg, result.Errors[0].Text)}
	}
	if len(result.OutputFiles) == 0 {
		return &pkgBuild{sig: sig, err: fmt.Errorf("prebundle %s: no output", entryPkg)}
	}

	// Map outputs by basename. esbuild names entry outputs e-<hash>.js (our
	// OutputPath) and shared chunks chunk-<hash>.js; both are imported by
	// basename within the package, so a flat basename map suffices. Rewrite
	// the entry/chunk cross-imports (relative "./chunk-X.js") to the served
	// /@pre/<pkg>/ path so the browser fetches siblings from this package.
	files := map[string]string{}
	for _, f := range result.OutputFiles {
		base := f.Path[strings.LastIndexByte(f.Path, '/')+1:]
		files[base] = rewritePkgChunkImports(string(f.Contents), entryPkg)
	}
	return &pkgBuild{files: files, sig: sig}
}

// pkgChunkImportRE matches a relative import/export of a sibling chunk or
// entry inside a package build, e.g. `from"./chunk-ABC.js"` or
// `from "./e-123.js"`. esbuild emits these between split outputs.
var pkgChunkImportRE = regexp.MustCompile(`(from\s*|import\s*|^\s*import\s+)(["'])\.?/?((?:chunk|e)-[A-Za-z0-9]+\.js)(["'])`)

// rewritePkgChunkImports rewrites a split output's relative sibling imports
// (./chunk-*.js, ./e-*.js) to absolute /@pre/<pkg>/ served paths so the
// browser fetches the shared chunk from this package's namespace.
func rewritePkgChunkImports(js, pkg string) string {
	prefix := PrebundlePrefix + pkg + "/"
	return pkgChunkImportRE.ReplaceAllString(js, `$1$2`+prefix+`$3$4`)
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
