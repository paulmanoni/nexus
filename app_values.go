package nexus

import (
	"io/fs"

	"github.com/paulmanoni/nexus/httpx"
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
}

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
