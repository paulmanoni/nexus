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
