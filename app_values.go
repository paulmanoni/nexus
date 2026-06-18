package nexus

import "github.com/paulmanoni/nexus/httpx"

// SetValue stashes a key/value on the app. Extensions use it to record
// boot-time state (typically in an fx.Invoke) that they must read back at
// request time — without depending on gin-middleware install ordering, which
// fx.Module route registration can run ahead of. Safe for concurrent use.
func (a *App) SetValue(key, value any) { a.extValues.Store(key, value) }

// Value returns a previously SetValue'd value, or (nil, false) if absent.
func (a *App) Value(key any) (any, bool) { return a.extValues.Load(key) }

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
