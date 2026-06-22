package nexus

import (
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
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
	bootIndex, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		if !devMode {
			// Production / standalone go-run: missing index.html
			// is a deploy bug. Fail loud so the broken artifact
			// surfaces at boot, not at first request.
			return fmt.Errorf("nexus: ServeFrontend: index.html not found — did the bundle build? (%w)", err)
		}
		// Dev mode: the operator is iterating on the app. Maybe
		// they haven't run `nexus add nexus-client/...` yet, or
		// haven't created islands.src/, or haven't built the
		// embedded bundle. Booting with a friendly placeholder
		// keeps the API + dashboard reachable so they can keep
		// working; visiting / shows them what to do next.
		log.Printf("nexus: ServeFrontend: no index.html — serving placeholder until you build a bundle (see http://<host>/ for instructions)")
		bootIndex = placeholderIndexHTML
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

	// Dev mode: mount the live-reload SSE channel + script so
	// the browser refreshes when a bundle file changes. Watches
	// the dev-root dir (NEXUS_DEV_ROOT, defaulting to "."); the
	// CLI's bundler is the producer of those file changes.
	// Production binaries never run this branch.
	if devMode {
		mountDevReload(app.engine, devReloadWatchDir(), app.devReloadExclude)
	}

	// Effective prefix is the concatenation of the deployment
	// prefix and the per-frontend mount path. Both are normalized
	// (leading slash, no trailing slash, "" or "/" become empty).
	// Walking back to "" when both are empty lets the NoRoute
	// handler skip the prefix-stripping branch entirely on simple
	// deployments.
	effectivePrefix := app.routePrefix + normalizeRoutePrefix(cfg.mountPath)

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

		// /index.html is a special case: http.FileServer redirects
		// it to "/" (its idea of the canonical form), which is
		// fine for browsers but surprising for callers that
		// scripted against /index.html. Serve the bytes directly
		// so the response is a plain 200 with the same body the
		// SPA fallback would return.
		if relPath == "/index.html" {
			c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
			c.Data(http.StatusOK, "text/html; charset=utf-8", readIndex())
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
			case strings.HasPrefix(relPath, "/assets/"):
				// Vite/Webpack/esbuild content-hashed names — cache
				// hard. The hash changes every release so a stale
				// cached entry can't outlive its filename.
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			}
			c.FileFromFS(relPath, httpFS)
			return
		}

		// SPA fallback. No-cache so a redeployed shell HTML is
		// picked up on the next reload — the browser asks every
		// time, the answer is fresh from the binary.
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "text/html; charset=utf-8", readIndex())
	})
	return nil
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
