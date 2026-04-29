package nexus

import (
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
)

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
	return rawOption{o: fx.Invoke(func(app *App) error {
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
func mountFrontend(app *App, fsys fs.FS, cfg *frontendConfig) error {
	indexHTML, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		return fmt.Errorf("nexus: ServeFrontend: index.html not found — did the bundle build? (%w)", err)
	}
	httpFS := http.FS(fsys)

	// Effective prefix is the concatenation of the deployment
	// prefix and the per-frontend mount path. Both are normalized
	// (leading slash, no trailing slash, "" or "/" become empty).
	// Walking back to "" when both are empty lets the NoRoute
	// handler skip the prefix-stripping branch entirely on simple
	// deployments.
	effectivePrefix := app.routePrefix + normalizeRoutePrefix(cfg.mountPath)

	app.engine.NoRoute(func(c *gin.Context) {
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
			c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
			return
		}

		// File request: anything with a "." in the path is treated
		// as an asset. SPA client routes (/users/123, /admin/edit)
		// are extensionless by convention. The rare client route
		// with a dot in it (an email-as-id, say) won't match here —
		// users can side-step by routing through a query string or
		// trailing slash, but the heuristic covers 99% of bundles.
		if strings.Contains(relPath, ".") {
			if strings.HasPrefix(relPath, "/assets/") {
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
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})
	return nil
}
