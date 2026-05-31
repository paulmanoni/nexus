// Package devserver implements viteless.Host backed by nexus's existing
// frontend machinery — the SFC compiler, the esbuild single-file transform,
// and the nexus.lock/.nexus-cache resolver. It is the seam that lets
// `nexus dev` serve the SPA unbundled (one module per URL, one Vue
// instance, real state-preserving HMR) instead of bundling, while
// `nexus build` keeps using the bundler unchanged.
//
// The Host plugs three operations into viteless:
//
//   - LoadModule  — bytes for a served URL: user source under Root, or a
//     cached dependency blob under DepPrefix.
//   - Transform   — one file → browser JS: .vue via the SFC compiler (+ an
//     HMR accept footer), .ts/.tsx/.jsx/.json via esbuild's
//     single-file Transform, .js/.mjs passthrough.
//   - ResolveImport — a specifier → the URL the browser should fetch:
//     relative/alias imports stay in the source tree; bare and
//     registry-internal imports go through the shared resolver
//     and are served from the cache under DepPrefix.
package devserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/viteless"
)

// Alias maps an import prefix to a filesystem directory, mirroring a
// tsconfig "paths" entry (e.g. {"@/", "<root>/src"} for "@/*": ["./src/*"]).
// The target dir is expected to live under Root so it can be served.
type Alias struct {
	Prefix string // e.g. "@/"
	Dir    string // absolute directory the prefix maps to
}

// Config configures a Host.
type Config struct {
	// Root is the directory served as the dev origin: index.html lives
	// here and a URL path "/x/y" maps to <Root>/x/y. Required.
	Root string

	// Resolver carries the lockfile + store + on-demand/dev-rewrite hooks
	// the shared resolver uses to turn bare specs into cached blob URLs.
	Resolver resolver.Options

	// Compiler compiles .vue SFCs. nil disables Vue (a .vue request then
	// surfaces a transform error / overlay).
	Compiler vue.SFCCompiler

	// Aliases are tsconfig-style path aliases resolved against the source
	// tree before the dependency resolver is consulted.
	Aliases []Alias

	// Env carries import.meta.env.<NAME> substitutions (the VITE_* vars
	// from .env files). Inlined as esbuild Defines during Transform so a
	// real app's `import.meta.env.VITE_API` reads the value at dev time.
	Env map[string]string

	// Mode is the dev mode injected as import.meta.env.MODE alongside the
	// DEV/PROD booleans. Typically "development".
	Mode string

	// DepPrefix is the URL namespace cached dependency blobs are served
	// under. Defaults to "/@dep/".
	DepPrefix string
}

// Host implements viteless.Host.
type Host struct {
	root      string
	depPrefix string
	res       resolver.Options
	compiler  vue.SFCCompiler
	aliases   []Alias
	defines   map[string]string

	// deps maps a served DepPrefix path → the canonical registry URL its
	// bytes are cached under. Populated by ResolveImport (which always
	// runs before the browser fetches the resolved URL) and read by
	// LoadModule; a deterministic encode/decode is the fallback.
	deps sync.Map
}

// New builds a Host from cfg.
func New(cfg Config) *Host {
	dp := cfg.DepPrefix
	if dp == "" {
		dp = "/@dep/"
	}
	return &Host{
		root:      cfg.Root,
		depPrefix: dp,
		res:       cfg.Resolver,
		compiler:  cfg.Compiler,
		aliases:   cfg.Aliases,
		defines:   buildDefines(cfg.Env, cfg.Mode),
	}
}

// buildDefines composes the import.meta.env.* substitution map handed to
// esbuild's Transform. Mirrors the bundler's dev Defines so unbundled dev
// and `nexus build` expose the same env surface: MODE/DEV/PROD plus each
// caller-supplied VITE_* var, all JSON-encoded to valid JS literals.
func buildDefines(env map[string]string, mode string) map[string]string {
	d := map[string]string{}
	if mode != "" {
		d["import.meta.env.MODE"] = jsLit(mode)
		d["import.meta.env.DEV"] = boolLit(mode == "development")
		d["import.meta.env.PROD"] = boolLit(mode == "production")
		d["import.meta.env.BASE_URL"] = jsLit("/")
	}
	for k, v := range env {
		d["import.meta.env."+k] = jsLit(v)
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

func jsLit(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// LoadModule returns the source bytes + kind for a served URL path.
func (h *Host) LoadModule(urlPath string) ([]byte, string, bool) {
	if strings.HasPrefix(urlPath, h.depPrefix) {
		return h.loadDep(urlPath)
	}
	return h.loadSource(urlPath)
}

// loadSource serves a file from the project source tree under Root.
func (h *Host) loadSource(urlPath string) ([]byte, string, bool) {
	clean := path.Clean(urlPath)
	if clean == "/" || clean == "." {
		clean = "/index.html"
	}
	// Reject traversal escapes: the cleaned path must stay rooted.
	if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return nil, "", false
	}
	fsPath := filepath.Join(h.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
	// Defense in depth: ensure the resolved path is still within Root.
	if rel, err := filepath.Rel(h.root, fsPath); err != nil || strings.HasPrefix(rel, "..") {
		return nil, "", false
	}
	body, err := os.ReadFile(fsPath)
	if err != nil {
		return nil, "", false
	}
	return body, kindForExt(strings.ToLower(path.Ext(clean))), true
}

// loadDep serves a cached dependency blob, reverse-mapping the served path
// to its canonical registry URL and reading it from the store.
func (h *Host) loadDep(urlPath string) ([]byte, string, bool) {
	canonical, ok := h.canonicalFor(urlPath)
	if !ok {
		return nil, "", false
	}
	blob, meta, err := h.res.Store.Get(canonical)
	if err != nil {
		// Cold miss: try an on-demand fetch through the shared resolver.
		if h.res.FetchOnDemand != nil {
			if c, ferr := h.res.FetchOnDemand(canonical); ferr == nil && c != "" {
				if blob, meta, err = h.res.Store.Get(c); err == nil {
					body, rerr := os.ReadFile(blob)
					if rerr == nil {
						return body, kindForContentType(meta.ContentType, canonical), true
					}
				}
			}
		}
		return nil, "", false
	}
	body, err := os.ReadFile(blob)
	if err != nil {
		return nil, "", false
	}
	return body, kindForContentType(meta.ContentType, canonical), true
}

// Transform compiles one source file to browser JS. viteless only calls
// this for "js"-kind modules; dispatch is by extension.
func (h *Host) Transform(urlPath string, src []byte) ([]byte, error) {
	// Strip any query (?t=…) viteless appends on hot re-imports.
	clean := urlPath
	if i := strings.IndexByte(clean, '?'); i >= 0 {
		clean = clean[:i]
	}
	ext := strings.ToLower(path.Ext(clean))
	switch ext {
	case ".vue":
		return h.transformVue(clean, src)
	case ".ts", ".tsx", ".jsx", ".json":
		return h.transformEsbuild(clean, src, ext)
	default:
		// .js / .mjs — already browser JS; passthrough (imports are
		// rewritten by viteless afterwards).
		return src, nil
	}
}

func (h *Host) transformVue(urlPath string, src []byte) ([]byte, error) {
	if h.compiler == nil {
		return nil, fmt.Errorf("no Vue SFC compiler wired for %s", urlPath)
	}
	res, err := h.compiler.Compile(string(src), urlPath)
	if err != nil {
		return nil, err
	}
	if len(res.Errors) > 0 {
		var b strings.Builder
		for i, e := range res.Errors {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s (%d:%d)", e.Message, e.Line, e.Column)
		}
		return nil, fmt.Errorf("%s", b.String())
	}
	// The compiled SFC already sets __sfc__.__hmrId and registers the
	// component with the Vue HMR runtime (createRecord). Append the
	// accept footer so an edited module hot-swaps the live component
	// in place (state preserved) instead of forcing a reload.
	return []byte(res.Code + vueHMRFooter), nil
}

// vueHMRFooter wires the standard Vue SFC hot-update path. It reads the
// runtime off globalThis (the dev Vue build installs it there) so it works
// in an unbundled module where a bare __VUE_HMR_RUNTIME__ would be
// undefined.
const vueHMRFooter = `
if (import.meta.hot) {
  import.meta.hot.accept((mod) => {
    const R = globalThis.__VUE_HMR_RUNTIME__;
    if (!mod || !R) { return; }
    const c = mod.default;
    if (c && c.__hmrId) { R.reload(c.__hmrId, c); }
  });
}
`

// transformEsbuild runs esbuild's single-file Transform to strip TS types /
// compile JSX / turn JSON into an ESM default export.
func (h *Host) transformEsbuild(urlPath string, src []byte, ext string) ([]byte, error) {
	loader := api.LoaderTS
	switch ext {
	case ".tsx":
		loader = api.LoaderTSX
	case ".jsx":
		loader = api.LoaderJSX
	case ".json":
		loader = api.LoaderJSON
	}
	r := api.Transform(string(src), api.TransformOptions{
		Loader:     loader,
		Format:     api.FormatESModule,
		Target:     api.ES2022,
		Sourcefile: urlPath,
		Sourcemap:  api.SourceMapInline,
		Define:     h.defines,
	})
	if len(r.Errors) > 0 {
		return nil, fmt.Errorf("%s: %s", urlPath, r.Errors[0].Text)
	}
	return r.Code, nil
}

// ResolveImport maps an import specifier to the URL the browser should
// fetch. importerURL is the served path of the importing module.
func (h *Host) ResolveImport(spec string, kind viteless.SpecKind, importerURL string) string {
	importerIsDep := strings.HasPrefix(importerURL, h.depPrefix)

	// Registry-internal imports: the importer is a cached dep blob, so a
	// relative/absolute/bare import resolves against its registry
	// siblings via the shared resolver.
	if importerIsDep {
		realImporter, _ := h.canonicalFor(importerURL)
		if u, ok, _ := h.res.ResolveURL(spec, realImporter); ok {
			return h.toDepPath(u)
		}
		return "" // leave untouched; the browser will surface the miss
	}

	// User code.
	switch kind {
	case viteless.SpecRelative:
		// Resolve against the importer's served directory; stays in the
		// source tree (served by loadSource).
		return path.Clean(path.Join(path.Dir(importerURL), spec))
	case viteless.SpecAbsolute:
		// A root-absolute path ("/foo") is already a served URL; an
		// absolute https:// URL is a registry reference.
		if strings.Contains(spec, "://") {
			if u, ok, _ := h.res.ResolveURL(spec, ""); ok {
				return h.toDepPath(u)
			}
		}
		return "" // "/foo" — leave as-is, loadSource handles it
	default: // SpecBare
		// tsconfig-style alias (e.g. "@/views/Foo.vue") → source tree.
		if served, ok := h.resolveAlias(spec); ok {
			return served
		}
		// Real package import → shared resolver → cached blob URL.
		if u, ok, _ := h.res.ResolveURL(spec, ""); ok {
			return h.toDepPath(u)
		}
		return ""
	}
}

// resolveAlias rewrites a tsconfig-style aliased import to its served path
// under Root. Returns ok=false when no alias prefix matches.
func (h *Host) resolveAlias(spec string) (string, bool) {
	for _, a := range h.aliases {
		if a.Prefix == "" || !strings.HasPrefix(spec, a.Prefix) {
			continue
		}
		rest := strings.TrimPrefix(spec, a.Prefix)
		abs := filepath.Join(a.Dir, filepath.FromSlash(rest))
		rel, err := filepath.Rel(h.root, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "", false // alias target outside Root — can't serve it
		}
		return "/" + filepath.ToSlash(rel), true
	}
	return "", false
}

// toDepPath encodes a canonical registry URL into a served DepPrefix path
// and records the mapping for the reverse lookup.
//
//	https://esm.sh/vue@3.5.13/es2022/vue.mjs → /@dep/https/esm.sh/vue@3.5.13/es2022/vue.mjs
//
// A "?query" (rare for stored dep URLs) is folded into the path so it
// survives as part of r.URL.Path; the recorded mapping is authoritative.
func (h *Host) toDepPath(canonical string) string {
	p := strings.Replace(canonical, "://", "/", 1)
	p = strings.ReplaceAll(p, "?", "/__q__/")
	sp := h.depPrefix + p
	h.deps.Store(sp, canonical)
	return sp
}

// canonicalFor reverses toDepPath. The recorded mapping wins; the
// deterministic decode is the fallback for a cold LoadModule.
func (h *Host) canonicalFor(servedPath string) (string, bool) {
	if v, ok := h.deps.Load(servedPath); ok {
		return v.(string), true
	}
	rest := strings.TrimPrefix(servedPath, h.depPrefix)
	if rest == servedPath {
		return "", false
	}
	rest = strings.ReplaceAll(rest, "/__q__/", "?")
	rest = strings.Replace(rest, "/", "://", 1)
	return rest, true
}

// kindForExt classifies a source file by extension into a viteless module
// kind. Unknown extensions are treated as assets (served as a URL export).
func kindForExt(ext string) string {
	switch ext {
	case ".html", ".htm":
		return "html"
	case ".css", ".scss", ".sass":
		return "css"
	case ".vue", ".ts", ".tsx", ".jsx", ".js", ".mjs", ".json":
		return "js"
	default:
		return "asset"
	}
}

// kindForContentType classifies a cached dependency blob. The URL's
// extension is the fallback when the Content-Type is missing/ambiguous.
func kindForContentType(ct, url string) string {
	lc := strings.ToLower(ct)
	switch {
	case strings.Contains(lc, "css"):
		return "css"
	case strings.Contains(lc, "javascript"), strings.Contains(lc, "ecmascript"), strings.Contains(lc, "typescript"), strings.Contains(lc, "json"):
		return "js"
	case strings.HasPrefix(lc, "font/"), strings.HasPrefix(lc, "image/"), strings.Contains(lc, "application/font"):
		return "asset"
	}
	switch strings.ToLower(path.Ext(stripURLQuery(url))) {
	case ".css":
		return "css"
	case ".woff", ".woff2", ".ttf", ".otf", ".eot", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".ico", ".avif":
		return "asset"
	default:
		return "js"
	}
}

func stripURLQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}
