package nexus

import (
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
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
// Mounted routes (under the deployment route prefix when one is
// set):
//
//	GET /                  → index.html
//	GET /index.html        → index.html
//	GET /<rootfile>        → that file (favicon.ico, robots.txt, …)
//	GET /<dir>/*filepath   → asset under that subdirectory
//	GET <anything else>    → index.html (SPA fallback via NoRoute)
//
// REST / GraphQL / WebSocket / dashboard routes are all registered
// before the NoRoute fallback runs, so they win on conflict — the
// SPA only catches requests that no real handler claimed. App
// boot fails fast when the FS lacks an index.html so a stale
// embed shows up at compile time, not at first request.
func ServeFrontend(fsys fs.FS, root string) Option {
	return rawOption{o: fx.Invoke(func(app *App) error {
		sub := fsys
		if root != "" {
			s, err := fs.Sub(fsys, root)
			if err != nil {
				return fmt.Errorf("nexus: ServeFrontend(root=%q): %w", root, err)
			}
			sub = s
		}
		return mountFrontend(app, sub)
	})}
}

// mountFrontend wires the SPA handlers onto the engine. Walks the
// FS root once at boot — every top-level file gets a literal route
// (so favicon.ico, robots.txt, etc. respond directly), every
// top-level directory gets a wildcard route (assets, static, etc.),
// and the framework's NoRoute hook catches everything else with a
// fall-through to index.html.
func mountFrontend(app *App, fsys fs.FS) error {
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return fmt.Errorf("nexus: ServeFrontend: index.html not found — did the bundle build? (%w)", err)
	}
	indexHandler := serveSPAFile(fsys, "index.html")
	app.engine.GET(app.PrefixPath("/"), indexHandler)
	app.engine.GET(app.PrefixPath("/index.html"), indexHandler)

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("nexus: ServeFrontend: read root: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == "index.html" {
			continue
		}
		if e.IsDir() {
			dir := name
			app.engine.GET(app.PrefixPath("/"+dir+"/*filepath"), func(c *gin.Context) {
				p := dir + "/" + strings.TrimPrefix(c.Param("filepath"), "/")
				serveSPAFile(fsys, p)(c)
			})
			continue
		}
		file := name
		app.engine.GET(app.PrefixPath("/"+file), serveSPAFile(fsys, file))
	}

	// SPA fallback for client-side routes (e.g. /users/123 in a
	// React Router app). NoRoute fires only when no other handler
	// matched — REST/GraphQL/WebSocket/dashboard registrations,
	// having mounted on Gin already, take precedence.
	app.engine.NoRoute(indexHandler)
	return nil
}

// serveSPAFile reads `name` from fsys on every request and writes
// it as the response body with a Content-Type derived from the
// extension. Reads aren't cached — embed.FS reads are essentially
// free (the bytes live in the binary's read-only segment) and the
// no-cache simplifies hot-reload during development when fsys is
// an os.DirFS.
func serveSPAFile(fsys fs.FS, name string) gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		c.Data(http.StatusOK, ct, data)
	}
}