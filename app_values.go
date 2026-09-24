package nexus

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/internal/vitehot"
)

// SetValue stashes a key/value on the app. Extensions use it to record
// boot-time state (typically in an fx.Invoke) that they must read back at
// request time — without depending on gin-middleware install ordering, which
// fx.Module route registration can run ahead of. Safe for concurrent use.
func (a *App) SetValue(key, value any) { a.extValues.Store(key, value) }

// Value returns a previously SetValue'd value, or (nil, false) if absent.
func (a *App) Value(key any) (any, bool) { return a.extValues.Load(key) }

// setFrontendSource records the built bundle ServeFrontend mounted, so
// extensions that read the bundle (not serve it) can discover it.
func (a *App) setFrontendSource(fsys fs.FS, root string) {
	a.frontendFS, a.frontendRoot = fsys, root
	// The hot file is read from disk, relative to the same dev root
	// ServeFrontend serves from under `nexus dev`; "." otherwise, which is
	// the project directory for a plain `go run .`.
	devRoot := os.Getenv(NexusDevRootEnv)
	if devRoot == "" {
		devRoot = "."
	}
	a.viteHot = vitehot.NewReader(filepath.Join(devRoot, root), func() bool {
		return vitehot.Enabled(IsDev(), a.Environment())
	})
}

// ViteHot returns the reader for the dev-server hot file nexus-vite-plugin
// writes, or nil when no frontend was registered. First-party extensions use
// it to find the Vite dev server instead of guessing — inertia's page shell is
// the other consumer besides ServeFrontend. Its Current reports a dev server
// only while that server is alive; a file left by one that has exited reads
// as absent (see vitehot.Reader.Current).
func (a *App) ViteHot() *vitehot.Reader { return a.viteHot }

// FrontendFS returns the built frontend bundle registered by ServeFrontend —
// the embed.FS and the dist root within it — and whether one was registered.
// An extension that needs to READ the bundle (e.g. inertia.Module resolving the
// Vite manifest) calls this instead of having the bundle passed to it again, so
// the app declares its frontend in exactly one place.
func (a *App) FrontendFS() (fsys fs.FS, root string, ok bool) {
	if a.frontendFS == nil {
		return nil, "", false
	}
	return a.frontendFS, a.frontendRoot, true
}

// ginAppKey is the gin.Context key under which buildGinHandler stashes the
// *App for renderers (see WithRenderer). A package-private string keeps it off
// the public surface while remaining accessible to AppFromGin.
const ginAppKey = "nexus.app"

// AppFromGin returns the *App associated with the current request, set by the
// framework before a ResponseRenderer runs. It lets a renderer reach per-app
// state (e.g. App.Value) that can't be threaded through the
// Render(c, result) signature. Returns (nil, false) outside a renderer-bearing
// request.
func AppFromGin(c *httpx.Ctx) (*App, bool) {
	v, ok := c.Get(ginAppKey)
	if !ok {
		return nil, false
	}
	app, ok := v.(*App)
	return app, ok
}

// FrontendDocument is the HTML document a server-rendered page is built from:
// the app's index.html as the browser should receive it right now. It lets a
// page renderer (inertia) keep everything the app put in index.html — title,
// meta, stylesheets, loaders — instead of synthesising a second document that
// has to repeat them.
type FrontendDocument struct {
	// HTML is the document. With a live Vite dev server announced by the hot
	// file it is Vite's transformed index.html, every root-relative asset URL
	// pointed at that server; otherwise it is the built bundle's index.html.
	HTML []byte
	// FromDevServer reports the first case: the scripts are live dev
	// modules, and the page must not be cached.
	FromDevServer bool
}

// ErrNoFrontendDocument means there is no index.html to render into right
// now: no frontend is registered, the build is module-only
// (nexus({ input: 'src/main.ts' }) emits no index.html), nothing is built yet
// (the placeholder page is not a document), or the live Vite dev server did
// not serve its index.html. Module-only is decided the same way in
// development as in production: a live dev server whose hot file declares no
// HTML entry stands for a module-only build, so this is returned even if that
// server's root holds an index.html. A page renderer then builds its own
// document.
var ErrNoFrontendDocument = errors.New("nexus: no frontend index.html to render into")

// FrontendDocument returns the document pages render into, or
// ErrNoFrontendDocument. Any other error means a hot file is present but
// can't be understood — malformed, an unknown schema version, an invalid
// origin — and should be shown to the developer rather than papered over. A
// hot file left by a dev server that has exited is not an error: it reads as
// absent, and the built index.html (or ErrNoFrontendDocument) is returned.
func (a *App) FrontendDocument(ctx context.Context) (FrontendDocument, error) {
	if a.frontendDoc == nil {
		return FrontendDocument{}, ErrNoFrontendDocument
	}
	return a.frontendDoc(ctx)
}

// FrontendMount is the URL path ServeFrontend serves the bundle under — the
// route prefix plus FrontendAt — or "" at the site root. A file at
// "assets/x.js" in the bundle is served at FrontendMount()+"/assets/x.js",
// which is what a renderer must link to rather than assuming "/".
func (a *App) FrontendMount() string { return a.frontendMount }
