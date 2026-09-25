package inertia

import "context"

// SSRRenderer turns a serialized Inertia page object into server-rendered head
// tags + body HTML. It's the seam for Inertia server-side rendering: the engine
// POSTs the page to a renderer on the initial (non-XHR) load and injects the
// result into the document shell, then the client hydrates it.
//
// The default implementation is extension/inertia/ssrhttp, which talks to a
// Node SSR server (the @inertiajs/server protocol) — in production at
// :13714, and in dev through the Vite dev server. Render must be safe for
// concurrent use; a returned error makes the engine fall back to client-side
// rendering (unless Config.SSRStrict is set).
type SSRRenderer interface {
	Render(ctx context.Context, page []byte) (SSRResult, error)
}

// SSRResult is the @inertiajs/server response: the head tags to hoist into
// <head> and the rendered app HTML to place inside the root element. A zero
// value (empty Body) means "nothing rendered" — the shell falls back to an
// empty root div for the client to mount.
type SSRResult struct {
	Head []string `json:"head"` // tags for <head> (title, meta, SSR <style>, …)
	Body string   `json:"body"` // server-rendered HTML for inside the root div
}

// devServerKey carries the Vite dev server a render is following.
type devServerKey struct{}

// withDevServer records origin as the dev server this render follows.
func withDevServer(ctx context.Context, origin string) context.Context {
	if origin == "" {
		return ctx
	}
	return context.WithValue(ctx, devServerKey{}, origin)
}

// DevServer returns the origin (scheme://host:port) of the Vite dev server
// the engine is following for this render — the one named by the hot file
// nexus-vite-plugin writes (App.ViteHot) — or "" when the page is served
// from the built bundle. The engine sets it on the context it passes to
// SSRRenderer.Render, so a renderer can use the dev server's SSR endpoint
// in development without being told where it is (see
// extension/inertia/ssrhttp).
func DevServer(ctx context.Context) string {
	s, _ := ctx.Value(devServerKey{}).(string)
	return s
}
