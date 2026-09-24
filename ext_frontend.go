package nexus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/internal/vitehot"
	"github.com/paulmanoni/nexus/internal/vitemanifest"
)

// init seeds the MIME type registry with the modern-web baseline
// ServeFrontend depends on. Go's mime.TypeByExtension() leans on
// the host's /etc/mime.types (or the Windows registry, or...) —
// systems where that file is missing or out-of-date return "" for
// .js / .css / .woff2 / .mjs, then http.ServeContent falls back
// to sniffing bytes via http.DetectContentType. The sniffer tags
// JS modules as "text/plain", which modern browsers refuse to
// load as ES modules.
//
// AddExtensionType prepends to the lookup table — it can't widen
// or replace the system mapping at runtime, but registering BEFORE
// any request hits ensures the right type wins for these specific
// extensions regardless of what the host advertises.
func init() {
	for ext, ct := range map[string]string{
		".js":    "application/javascript; charset=utf-8",
		".mjs":   "application/javascript; charset=utf-8",
		".cjs":   "application/javascript; charset=utf-8",
		".css":   "text/css; charset=utf-8",
		".map":   "application/json; charset=utf-8", // sourcemaps
		".json":  "application/json; charset=utf-8",
		".svg":   "image/svg+xml",
		".woff":  "font/woff",
		".woff2": "font/woff2",
		".ttf":   "font/ttf",
		".otf":   "font/otf",
		".eot":   "application/vnd.ms-fontobject",
		".webp":  "image/webp",
		".avif":  "image/avif",
	} {
		_ = mime.AddExtensionType(ext, ct)
	}
}

// NexusDevEnv signals dev mode to the framework. When set to "1",
// ServeFrontend reads files from disk (os.DirFS) instead of the
// supplied embed.FS, so a watching frontend toolchain (vite build
// --watch, esbuild --watch) can refresh the served bundle without
// recompiling Go. nexus dev sets it on the spawned process env.
const NexusDevEnv = "NEXUS_DEV"

// NexusDevRootEnv overrides the disk root used in dev mode. Defaults
// to "." (the binary's CWD), which matches how //go:embed paths are
// declared. nexus dev sets it to the resolved target directory so
// users running from a non-project CWD still resolve correctly.
const NexusDevRootEnv = "NEXUS_DEV_ROOT"

// ServeFrontend mounts a built single-page-app bundle from an
// embedded filesystem. The classic shape:
//
//	//go:embed all:web/dist
//	var webFS embed.FS
//
//	nexus.Run(nexus.Config{...},
//	    nexus.ServeFrontend(webFS, "web/dist"),
//	    uaa.Module,
//	    interview.Module,
//	)
//
// The `root` argument is the directory inside fsys that holds
// index.html plus the asset subdirectories — typically the same
// path passed to //go:embed minus the `all:` prefix. Pass "" when
// fsys is already rooted at the dist directory (e.g. after
// fs.Sub).
//
// Pass nexus.FrontendAt("/admin") (or any sub-path) to mount the
// SPA under a sub-path instead of at the deployment root — useful
// when REST/GraphQL live at /api/* and the frontend should answer
// at /admin/* on the same listener.
//
// Behavior (under the deployment route prefix when one is set,
// then the FrontendAt mount path when one is set):
//
//   - Files with an extension (foo.js, /assets/main.css,
//     /favicon.ico) are served from the embed.FS directly. Files
//     under /assets/ get an immutable far-future Cache-Control —
//     Vite, Webpack, and esbuild all stamp content hashes into
//     filenames there, so the cached copy can never go stale.
//   - Anything else is treated as a client-side route and gets
//     index.html with a no-cache header (so an updated bundle is
//     picked up on the next reload, not held for a year).
//   - REST / GraphQL / WebSocket / dashboard routes are registered
//     before the NoRoute hook fires, so they win on conflict.
//
// App boot fails fast when the FS lacks an index.html so a stale
// or unbuilt bundle surfaces at start time, not at first request.
func ServeFrontend(fsys fs.FS, root string, opts ...FrontendOption) Option {
	cfg := &frontendConfig{}
	for _, o := range opts {
		o.applyToFrontend(cfg)
	}
	// Capture the original (production embed) bundle before the dev swap, so
	// extensions that READ the bundle — inertia.Module's manifest resolver —
	// can discover it via App.FrontendFS() without it being passed twice.
	srcFS, srcRoot := fsys, root
	// Dev-mode swap: read from disk instead of embed.FS so a
	// watching frontend toolchain (vite build --watch) refreshes
	// the served bundle without recompiling Go. Same `root`
	// semantics — fs.Sub still narrows to the dist directory.
	if os.Getenv(NexusDevEnv) == "1" {
		dvr := os.Getenv(NexusDevRootEnv)
		if dvr == "" {
			dvr = "."
		}
		fsys = os.DirFS(dvr)
	}
	return rawOption{o: di.Invoke(func(app *App) error {
		app.setFrontendSource(srcFS, srcRoot)
		sub := fsys
		if root != "" {
			s, err := fs.Sub(fsys, root)
			if err != nil {
				return fmt.Errorf("nexus: ServeFrontend(root=%q): %w", root, err)
			}
			sub = s
		}
		return mountFrontend(app, sub, cfg)
	})}
}

// FrontendOption tunes a ServeFrontend call. Returned by helpers
// like FrontendAt; users don't construct these directly.
type FrontendOption interface {
	applyToFrontend(*frontendConfig)
}

type frontendConfig struct {
	mountPath string
}

type frontendMountAt struct{ path string }

func (m frontendMountAt) applyToFrontend(c *frontendConfig) { c.mountPath = m.path }

// FrontendAt sets a sub-path the SPA is served under, in addition
// to the deployment-wide route prefix. The two compose: deployment
// prefix /v1 + FrontendAt("/admin") → SPA at /v1/admin/. Useful
// when API endpoints live at the deployment root and the frontend
// should answer on a sibling path. Empty / "/" mean the SPA mounts
// directly under the deployment prefix (the default).
//
// Trailing slashes are trimmed; a leading slash is added if
// missing. Pass "/admin" or "admin" — both resolve to "/admin".
func FrontendAt(path string) FrontendOption {
	return frontendMountAt{path: path}
}

// mountFrontend wires a single NoRoute handler that dispatches by
// path shape: files (anything with a `.`) come from the embed.FS,
// extensionless paths fall back to index.html for SPA routing.
// One handler instead of per-file/per-dir registrations keeps the
// engine route table small and lets the dispatcher own all the
// caching policy in one place.
//
// index.html policy: in production we read once at boot and serve
// the cached bytes (assets are content-hashed, the shell never
// changes between deploys, no point hitting disk). In dev mode
// (NEXUS_DEV=1) the disk-FS swap above is meaningless if we still
// cache the shell — vite writes a fresh dist/index.html with new
// asset hashes on each rebuild, and stale cached bytes would point
// at deleted assets. The dev path re-reads index.html per request
// so a frontend rebuild becomes visible on the next refresh.
func mountFrontend(app *App, fsys fs.FS, cfg *frontendConfig) error {
	devMode := os.Getenv(NexusDevEnv) == "1"
	// The Vite manifest drives cache policy, and proves a build happened
	// even when that build produced no index.html: an app whose entry is a
	// module — nexus({ input: 'src/main.ts' }) for an Inertia app — builds
	// pages, not a shell.
	man, manErr := vitemanifest.Load(fsys, ".")
	bootIndex, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		switch {
		case manErr == nil:
			// Built, but shell-less: there is no page to fall back to,
			// so client routes 404 rather than render an empty document.
			log.Printf("nexus: ServeFrontend: bundle has no index.html (built from %s) — unknown routes return 404", man.Path)
			bootIndex = nil
		case vitehot.Enabled(devMode, app.Environment()):
			// Development — nexus dev, or environment = "development" with a
			// plain go run — before anything is built. The frontend may be a
			// Vite dev server the hot file points at, so keep the API and
			// dashboard reachable and explain on / what to do next.
			log.Printf("nexus: ServeFrontend: no index.html — serving placeholder until you build a bundle (see http://<host>/ for instructions)")
			bootIndex = placeholderIndexHTML
		default:
			// Production: a bundle with neither a shell nor a manifest was
			// never built. Fail loud so the broken artifact surfaces at
			// boot, not at first request.
			return fmt.Errorf("nexus: ServeFrontend: the bundle has neither index.html nor a Vite manifest — did it build? (%w)", err)
		}
	}
	readIndex := func() []byte {
		if !devMode {
			return bootIndex
		}
		fresh, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			// Vite mid-rebuild may briefly delete/rewrite index.html;
			// fall back to the boot copy rather than 500ing.
			return bootIndex
		}
		return fresh
	}
	httpFS := http.FS(fsys)

	// Production cache policy comes from the build, not from path
	// conventions: see assetCacheControl.
	var immutable map[string]bool
	if manErr == nil {
		immutable = man.Immutable()
	}
	etags := &etagCache{fsys: fsys}
	indexETag := ""
	if bootIndex != nil {
		indexETag = contentETag(bootIndex)
	}

	// Dev mode: mount the live-reload SSE channel + script so
	// the browser refreshes when a bundle file changes. Watches
	// the dev-root dir (NEXUS_DEV_ROOT, defaulting to "."); the
	// CLI's bundler is the producer of those file changes.
	// Production binaries never run this branch.
	if devMode {
		mountDevReload(app.engine, devReloadWatchDir(), app.devReloadExclude, func() bool {
			if h, _ := app.ViteHot().Current(); h != nil {
				return true
			}
			return os.Getenv("NEXUS_VITE_DEV") != ""
		})
	}

	// Effective prefix is the concatenation of the deployment
	// prefix and the per-frontend mount path. Both are normalized
	// (leading slash, no trailing slash, "" or "/" become empty).
	// Walking back to "" when both are empty lets the NoRoute
	// handler skip the prefix-stripping branch entirely on simple
	// deployments.
	effectivePrefix := app.routePrefix + normalizeRoutePrefix(cfg.mountPath)
	app.frontendMount = effectivePrefix

	// serveIndex answers every index.html response — the SPA fallback and a
	// direct /index.html. While a Vite dev server has announced itself in
	// the hot file, the page loads its modules from that server; otherwise
	// it is the bundle's index.html, exactly as before.
	dev := &devIndex{}
	app.frontendDoc = func(ctx context.Context) (FrontendDocument, error) {
		if h, err := app.ViteHot().Current(); err != nil {
			return FrontendDocument{}, err
		} else if h != nil {
			// Only Vite's own transformed page is a faithful document for
			// a dev server; if it can't be fetched, the caller builds its
			// own rather than rendering into a stale or placeholder copy.
			body, ferr := fetchDevIndex(ctx, h)
			if ferr != nil {
				if ctx.Err() == nil {
					dev.logFetchFailure(h, ferr)
				}
				return FrontendDocument{}, ErrNoFrontendDocument
			}
			return FrontendDocument{HTML: body, FromDevServer: true}, nil
		}
		page := readIndex()
		if len(page) == 0 || bytes.Equal(page, placeholderIndexHTML) {
			return FrontendDocument{}, ErrNoFrontendDocument
		}
		return FrontendDocument{HTML: page}, nil
	}
	serveIndex := func(c *httpx.Ctx) {
		// A live dev server comes first in every mode, not only under
		// nexus dev: `go run .` with environment = "development" honours
		// the hot file too.
		if body, status, ok := dev.render(c.Request.Context(), app.ViteHot(), readIndex); ok {
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.Data(status, "text/html; charset=utf-8", body)
			return
		}
		page := readIndex()
		if len(page) == 0 {
			// A shell-less build: nothing to fall back to.
			c.Status(http.StatusNotFound)
			return
		}
		if devMode {
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.Data(http.StatusOK, "text/html; charset=utf-8", page)
			return
		}
		// Stored but revalidated on every use. The shell is tiny and must
		// never be stale after a deploy, but an unchanged one costs a 304
		// instead of the whole document — no-store threw that away.
		c.Header("Cache-Control", "no-cache")
		c.Header("ETag", indexETag)
		if etagMatches(c.Request.Header.Get("If-None-Match"), indexETag) {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", bootIndex)
	}

	app.engine.NoRoute(func(c *httpx.Ctx) {
		urlPath := c.Request.URL.Path

		// When a prefix is set, only paths under it are SPA
		// candidates. Unprefixed misses 404 the way they would on
		// a non-SPA deployment — keeps the SPA from accidentally
		// swallowing requests that belong to a different mount on
		// the same listener (REST API at /api, SPA at /admin, for
		// example).
		relPath := urlPath
		if effectivePrefix != "" {
			if !strings.HasPrefix(urlPath, effectivePrefix) {
				c.Status(http.StatusNotFound)
				return
			}
			relPath = strings.TrimPrefix(urlPath, effectivePrefix)
			if relPath == "" {
				relPath = "/"
			}
		}

		// The hot file describes a local dev server and its pid. It
		// sits inside the served tree, and `//go:embed all:web/dist`
		// would carry a stale one into a production binary, so it is
		// never served — in any mode.
		if isHotFilePath(relPath) {
			c.Status(http.StatusNotFound)
			return
		}

		// /index.html is a special case: http.FileServer redirects
		// it to "/" (its idea of the canonical form), which is
		// fine for browsers but surprising for callers that
		// scripted against /index.html. Serve the bytes directly
		// so the response is a plain 200 with the same body the
		// SPA fallback would return.
		if relPath == "/index.html" {
			serveIndex(c)
			return
		}

		// File request: anything with a "." in the path is treated
		// as an asset. SPA client routes (/users/123, /admin/edit)
		// are extensionless by convention. The rare client route
		// with a dot in it (an email-as-id, say) won't match here —
		// users can side-step by routing through a query string or
		// trailing slash, but the heuristic covers 99% of bundles.
		if strings.Contains(relPath, ".") {
			switch {
			case devMode:
				// Dev mode: NEVER let the browser cache assets.
				// The bundler rewrites main.js / main.css on every
				// save and the dev-reload shim triggers
				// location.reload(); without an explicit no-cache
				// header the browser heuristically caches the
				// previous bytes and the reload serves stale code.
				// Combined no-cache + no-store + must-revalidate is
				// the only combination that works across Chrome /
				// Firefox / Safari in 2026.
				c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
				c.Header("Pragma", "no-cache")
				c.Header("Expires", "0")
			default:
				rel := strings.TrimPrefix(path.Clean(relPath), "/")
				c.Header("Cache-Control", assetCacheControl(rel, immutable))
				// http.FileServer answers If-None-Match from this header,
				// which is what makes the revalidating responses cheap:
				// embedded files have no modification time, so without an
				// ETag they could never be answered with a 304.
				if tag := etags.get(rel); tag != "" {
					c.Header("ETag", tag)
				}
			}
			c.FileFromFS(relPath, httpFS)
			return
		}

		// SPA fallback. No-cache so a redeployed shell HTML is
		// picked up on the next reload — the browser asks every
		// time, the answer is fresh from the binary.
		serveIndex(c)
	})
	return nil
}

// hotFileURLPath is the hot file's location under the served root.
var hotFileURLPath = "/" + vitehot.Dir + "/" + vitehot.File

// isHotFilePath reports whether a request path (relative to the SPA mount)
// names the hot file. The path is cleaned the way http.FileServer cleans it,
// and compared case-insensitively because the dev disk may be (macOS is).
func isHotFilePath(rel string) bool {
	return strings.EqualFold(path.Clean("/"+rel), hotFileURLPath)
}

// devIndexTimeout bounds the per-request fetch of the transformed
// index.html from the Vite dev server. Vite answers it from memory in
// milliseconds; a server that takes longer is wedged, and the on-disk
// fallback beats a hung page.
const devIndexTimeout = 3 * time.Second

// devIndexClient fetches the dev server's index.html. Proxies are bypassed:
// the dev server is a local process, never something to route through a
// corporate proxy picked up from the environment.
var devIndexClient = &http.Client{
	Timeout:   devIndexTimeout,
	Transport: &http.Transport{Proxy: nil},
}

// devIndex renders index.html against the Vite dev server named by the hot
// file. It holds no page cache — the dev server may restart on another port
// between two requests — only the last fetch failure it logged, so a dev
// server that keeps refusing is reported once, not on every page load.
type devIndex struct {
	mu         sync.Mutex
	lastLogged string
}

// render returns the dev page and its status, or ok=false when there is no
// dev server to use and the caller should serve the bundle's index.html.
//
//   - hot file present: Vite's own transformed index.html (so every
//     transformIndexHtml tag and the /@vite/client injection are there),
//     with root-relative asset URLs pointed at the dev server; when that
//     fetch fails, the on-disk index.html with the client and entries
//     injected.
//   - hot file present but unusable (malformed, unknown schema, left by a
//     dead process): an error page naming the problem. Serving the stale
//     bundle instead would look like "my edits don't show up".
//   - no hot file, or hot files disabled (production): ok=false.
func (d *devIndex) render(ctx context.Context, r *vitehot.Reader, readIndex func() []byte) ([]byte, int, bool) {
	if r == nil {
		return nil, 0, false
	}
	h, err := r.Current()
	if err != nil {
		return hotErrorPage(err), http.StatusServiceUnavailable, true
	}
	if h == nil {
		return nil, 0, false
	}
	body, ferr := fetchDevIndex(ctx, h)
	if ferr == nil {
		d.mu.Lock()
		d.lastLogged = ""
		d.mu.Unlock()
		return body, http.StatusOK, true
	}
	if ctx.Err() == nil {
		d.logFetchFailure(h, ferr)
	}
	return devIndexFromDisk(readIndex(), h), http.StatusOK, true
}

func (d *devIndex) logFetchFailure(h *vitehot.Hot, err error) {
	msg := err.Error()
	d.mu.Lock()
	defer d.mu.Unlock()
	if msg == d.lastLogged {
		return
	}
	d.lastLogged = msg
	log.Printf("nexus: ServeFrontend: could not load index.html from the Vite dev server at %s (%v) — serving the on-disk index.html with the Vite client injected instead", h.Origin, err)
}

// fetchDevIndex asks the dev server for index.html. Vite runs its
// transformIndexHtml pipeline on that request, so the body carries what the
// plugins inject. Vite writes root-relative URLs (with its base already
// applied) — it does not prefix server.origin in HTML — and the page is
// served from the Go origin, so they are made absolute against the dev
// server here.
func fetchDevIndex(ctx context.Context, h *vitehot.Hot) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, devIndexTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL("index.html"), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	resp, err := devIndexClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", req.URL, resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "text/html") {
		return nil, fmt.Errorf("GET %s: content-type %q, want text/html", req.URL, ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	origin := strings.TrimRight(h.Origin, "/")
	return absolutizeDevHTML(body, func(p string) string { return origin + p }), nil
}

var (
	// devAssetTagRE matches the opening tags whose URL attributes load
	// something the dev server serves. Anchors and forms are left alone:
	// navigation belongs on the Go origin.
	devAssetTagRE = regexp.MustCompile(`(?is)<(?:script|link|img)\b[^>]*>`)
	// devURLAttrRE captures a quoted src/href value that is root-relative.
	devURLAttrRE = regexp.MustCompile(`(?is)(\s(?:src|href)\s*=\s*)(?:"(/[^"]*)"|'(/[^']*)')`)
	// devScriptRE matches a whole script element, for inline module bodies.
	devScriptRE = regexp.MustCompile(`(?is)(<script\b[^>]*>)(.*?)(</script\s*>)`)
	// devImportRE captures a root-relative specifier after `from`,
	// `import` or `import(` — static imports, re-exports, dynamic imports.
	devImportRE     = regexp.MustCompile(`(\b(?:from|import)\s*\(?\s*)(?:"(/[^"]*)"|'(/[^']*)')`)
	devModuleTypeRE = regexp.MustCompile(`(?i)\stype\s*=\s*["']?module\b`)
	devSrcAttrRE    = regexp.MustCompile(`(?i)\ssrc\s*=`)
	devHeadOpenRE   = regexp.MustCompile(`(?is)<head\b[^>]*>`)
	devBodyCloseRE  = regexp.MustCompile(`(?i)</body\s*>`)
)

// devKeepOnGoOrigin reports whether a root-relative URL stays on the Go
// origin: protocol-relative URLs are not paths at all, and /__nexus is the
// app's own surface (dashboard, live-reload shim), never the dev server's.
func devKeepOnGoOrigin(p string) bool {
	return strings.HasPrefix(p, "//") || p == "/__nexus" || strings.HasPrefix(p, "/__nexus/")
}

// rewriteQuoted replaces the root-relative URL a devURLAttrRE/devImportRE
// match captured (group 2 when double-quoted, 3 when single-quoted).
func rewriteQuoted(re *regexp.Regexp, s string, to func(string) string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		sub := re.FindStringSubmatch(m)
		q, v := `"`, sub[2]
		if v == "" {
			q, v = `'`, sub[3]
		}
		if devKeepOnGoOrigin(v) {
			return m
		}
		return sub[1] + q + to(v) + q
	})
}

// absolutizeDevHTML points every root-relative asset URL in a page at the
// dev server: src/href on script, link and img tags, and import specifiers
// inside inline module scripts (a plugin-injected preamble such as
// `import RefreshRuntime from "/@react-refresh"` resolves against the
// document, which is the Go origin).
func absolutizeDevHTML(page []byte, to func(string) string) []byte {
	s := devScriptRE.ReplaceAllStringFunc(string(page), func(m string) string {
		parts := devScriptRE.FindStringSubmatch(m)
		open, body, closing := parts[1], parts[2], parts[3]
		if !devModuleTypeRE.MatchString(open) || devSrcAttrRE.MatchString(open) {
			return m
		}
		return open + rewriteQuoted(devImportRE, body, to) + closing
	})
	s = devAssetTagRE.ReplaceAllStringFunc(s, func(tag string) string {
		return rewriteQuoted(devURLAttrRE, tag, to)
	})
	return []byte(s)
}

// devIndexFromDisk is the fallback when the dev server's index.html cannot
// be fetched: the page ServeFrontend would have served, with the Vite
// client injected into <head> and root-relative module scripts loaded from
// the dev server. When the hot file declares a module entry and none of
// those scripts is it — a placeholder page, or a built index.html that
// references hashed bundles — the entry is added, so the page still boots
// the live source. (An SPA whose declared entry is index.html itself has no
// module entry; its page already carries its scripts.)
func devIndexFromDisk(page []byte, h *vitehot.Hot) []byte {
	entry := strings.TrimPrefix(h.ModuleEntry(), "/")
	hasEntry := false
	s := devAssetTagRE.ReplaceAllStringFunc(string(page), func(tag string) string {
		if !strings.HasPrefix(strings.ToLower(tag), "<script") || !devModuleTypeRE.MatchString(tag) {
			return tag
		}
		return rewriteQuoted(devURLAttrRE, tag, func(p string) string {
			if entry != "" && strings.TrimPrefix(p, "/") == entry {
				hasEntry = true
			}
			return h.URL(p)
		})
	})

	if !strings.Contains(s, h.ClientURL()) {
		client := `<script type="module" src="` + html.EscapeString(h.ClientURL()) + `"></script>`
		if loc := devHeadOpenRE.FindStringIndex(s); loc != nil {
			s = s[:loc[1]] + "\n" + client + s[loc[1]:]
		} else {
			s = client + "\n" + s
		}
	}
	if entry != "" && !hasEntry {
		tag := `<script type="module" src="` + html.EscapeString(h.URL(entry)) + `"></script>` + "\n"
		if loc := devBodyCloseRE.FindStringIndex(s); loc != nil {
			s = s[:loc[0]] + tag + s[loc[0]:]
		} else {
			s += "\n" + tag
		}
	}
	return []byte(s)
}

// hotErrorPage explains a hot file that exists but cannot be followed.
func hotErrorPage(err error) []byte {
	return []byte(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>nexus — Vite dev server unavailable</title>
<style>
  body { margin:0; padding:40px 24px; background:#f8fafc; color:#0f172a;
         font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  main { max-width:720px; margin:0 auto; background:#fff; border:1px solid #fecaca;
         border-radius:12px; padding:28px 32px; }
  h1 { margin:0 0 8px; font-size:20px; color:#b91c1c; }
  pre { background:#0f172a; color:#f1f5f9; border-radius:8px; padding:14px 16px;
        white-space:pre-wrap; word-break:break-word;
        font: 13px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace; }
</style></head>
<body><main>
<h1>The Vite dev server's hot file can't be used</h1>
<p>nexus-vite-plugin wrote a hot file announcing a dev server, but it is not usable, so this page was not served from the stale build instead:</p>
<pre>` + html.EscapeString(err.Error()) + `</pre>
<p>Start the dev server again (<code>npm run dev</code>, or <code>nexus dev</code>), or delete the file if no dev server should be running.</p>
</main></body>
</html>
`)
}

// placeholderIndexHTML is the friendly fallback served when
// ServeFrontend boots in dev mode without an index.html. Tells
// the operator their API + dashboard are up + working and shows
// the canonical "set up a frontend" recipe — no need to leave
// the browser to figure out next steps.
//
// Inlined as a byte slice so the dev-fallback path stays a
// zero-allocation pass-through (cached at boot like the real
// index.html).
var placeholderIndexHTML = []byte(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>nexus — no frontend yet</title>
<style>
  :root { --ink:#0f172a; --mute:#64748b; --accent:#4f46e5; --bg:#f8fafc; --line:#e2e8f0; }
  * { box-sizing:border-box }
  html, body { margin:0; padding:0; background:var(--bg); color:var(--ink);
               font: 15px/1.6 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  body { display:flex; align-items:center; justify-content:center; min-height:100vh; padding:24px }
  main { max-width: 640px; background:#fff; padding:36px 44px; border-radius:14px;
         border:1px solid var(--line); box-shadow: 0 4px 14px rgba(15,23,42,.05) }
  h1 { margin:0 0 6px; font-size:22px; display:flex; align-items:center; gap:10px }
  h1 .logo { width:32px; height:32px; border-radius:8px;
             background: linear-gradient(135deg, #6366f1, #ec4899);
             display:inline-flex; align-items:center; justify-content:center;
             color:#fff; font-size:16px }
  p.lead { color:var(--mute); margin:0 0 22px }
  h2 { font-size:13px; color:var(--mute); letter-spacing:.06em; text-transform:uppercase;
       margin:24px 0 10px; font-weight:700 }
  ol { padding-left:20px; margin:0 0 6px }
  ol li { margin:8px 0 }
  code { background:#f1f5f9; border:1px solid var(--line); border-radius:4px;
         padding:1px 6px; font-size:13.5px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace }
  pre { background:#0f172a; color:#f1f5f9; border-radius:8px; padding:14px 16px; margin:10px 0 0;
        font: 13px/1.6 ui-monospace, SFMono-Regular, Menlo, monospace; overflow:auto }
  .links { margin-top:24px; display:flex; flex-wrap:wrap; gap:8px }
  .links a { background:#eef2ff; color:var(--accent); padding:6px 12px; border-radius:6px;
             text-decoration:none; font-weight:500; font-size:13px }
  .links a:hover { background:#e0e7ff }
  .small { font-size:12.5px; color:var(--mute); margin-top:14px }
</style>
</head>
<body>
<main>
  <h1><span class="logo">N</span> No frontend yet</h1>
  <p class="lead">Your nexus app is running. <strong>API, GraphQL, WebSockets, and the dashboard work right now</strong> — only the SPA shell is missing.</p>

  <h2>Pick one</h2>
  <ol>
    <li>Use the typed SDK directly:
<pre>nexus add nexus-client/vue     # or /react</pre>
    </li>
    <li>Build your own SPA into the embedded bundle path your <code>main.go</code> passes to <code>nexus.ServeFrontend(...)</code>.</li>
    <li>Set up the islands pipeline: create <code>islands.src/</code> with one entry per page and rerun <code>nexus dev</code>.</li>
  </ol>

  <div class="links">
    <a href="/__nexus/">Dashboard</a>
    <a href="/__nexus/openapi/ui">OpenAPI</a>
    <a href="/__nexus/client/manifest.json">Client manifest</a>
  </div>
  <p class="small">This placeholder shows only in dev mode. Production binaries fail to boot if <code>index.html</code> is missing.</p>
</main>
</body>
</html>
`)

// assetCacheControl is the production Cache-Control for a file in the bundle
// (rel is relative to the bundle root). A file is cached forever only when the
// Vite manifest lists it as build output AND its name carries a content hash:
// the manifest proves Vite produced it, and the hash proves new content gets a
// new name. Everything else — public/ copies such as favicon.ico, anything
// under a hash-free name — is stored but revalidated on each use, which its
// ETag turns into a 304.
//
// This replaced a rule keyed on the /assets/ prefix, which cached an unhashed
// public/assets/logo.png for a year (so a changed logo never reached returning
// visitors) and cached nothing when build.assetsDir was renamed.
//
// Without a manifest (a bundle not built by Vite) the prefix convention is
// the only signal left, so it still applies — but only to names that carry a
// hash.
func assetCacheControl(rel string, immutable map[string]bool) string {
	const forever = "public, max-age=31536000, immutable"
	switch {
	case immutable != nil:
		if immutable[rel] {
			return forever
		}
	case strings.HasPrefix(rel, "assets/") && vitemanifest.IsHashedName(rel):
		return forever
	}
	return "public, no-cache"
}

// etagCache computes a bundle file's ETag on first request and reuses it while
// the file is unchanged. An embedded bundle cannot change, so its entries are
// computed once; a bundle served from disk is re-checked by size and
// modification time, so a replaced file gets a new tag.
type etagCache struct {
	fsys fs.FS
	m    sync.Map // rel → etagEntry
}

type etagEntry struct {
	size int64
	mod  time.Time
	tag  string
}

func (e *etagCache) get(rel string) string {
	fi, err := fs.Stat(e.fsys, rel)
	if err != nil || fi.IsDir() {
		return ""
	}
	if v, ok := e.m.Load(rel); ok {
		if ent := v.(etagEntry); ent.size == fi.Size() && ent.mod.Equal(fi.ModTime()) {
			return ent.tag
		}
	}
	f, err := e.fsys.Open(rel)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	tag := `"` + hex.EncodeToString(h.Sum(nil)[:8]) + `"`
	e.m.Store(rel, etagEntry{size: fi.Size(), mod: fi.ModTime(), tag: tag})
	return tag
}

// contentETag is a strong ETag for a byte slice.
func contentETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// etagMatches implements If-None-Match: a list of tags, weak tags compared
// weakly (as RFC 9110 requires for this header), or "*".
func etagMatches(header, tag string) bool {
	if header == "" || tag == "" {
		return false
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimSpace(t)
		if t == "*" || strings.TrimPrefix(t, "W/") == tag {
			return true
		}
	}
	return false
}
