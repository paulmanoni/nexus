package nexus

import "github.com/gin-gonic/gin"

// ResponseRenderer overrides how a REST endpoint's *successful* return value
// is written to the response. By default an AsRest handler's return is encoded
// with c.JSON(...); attaching a renderer via WithRenderer hands that final
// write to custom code instead.
//
// This is the single extension point the Inertia integration
// (github.com/paulmanoni/nexus/extension/inertia) builds on: a page handler
// stays an ordinary reflective handler returning a typed props struct, and the
// renderer wraps that struct into the Inertia page object — emitting JSON for
// XHR visits or an HTML document shell for full loads. Keeping the hook here
// (rather than re-implementing handler reflection in the extension) means
// params binding, validation, DI, tracing, and metrics behave identically to
// every other REST endpoint.
//
// Render receives the live *gin.Context (headers, request URL, the
// ResponseWriter) and the handler's return value. Returning an error is
// reported through gin.Context.Error and, if nothing has been written yet,
// produces a 500 — same as a handler error. Only successful returns reach a
// renderer; the error path is untouched.
type ResponseRenderer interface {
	Render(c *gin.Context, result any) error
}

// WithRenderer attaches a ResponseRenderer to a single AsRest registration,
// replacing the default JSON success write. It is a REST-only option (GraphQL
// and WebSocket returns are encoded by their own transports).
//
//	nexus.AsRest("GET", "/users", NewListUsers, nexus.WithRenderer(myRenderer))
//
// Most apps never call this directly — higher-level helpers such as
// inertia.Page wire the renderer for you.
func WithRenderer(r ResponseRenderer) RestOption {
	return restOptionFn(func(c *restConfig) { c.renderer = r })
}
