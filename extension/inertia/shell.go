package inertia

import (
	"html"
	"strings"
)

// shell synthesises the full HTML document for a non-XHR (initial) page load
// when there is no index.html to render into (see pageDocument and
// pageTemplate.render), with assetHead (the tags that load the client app —
// see assets) in <head>. The
// serialized page object is embedded in the root element's data-page attribute
// — the Inertia client adapter reads it on boot to mount (or hydrate) the first
// component without a follow-up request. dataPage is HTML-attribute-escaped so
// the JSON's quotes and angle brackets can't break out of the attribute.
//
// When ssr carries server-rendered output, its head tags are hoisted into
// <head> and its body is placed inside the root div (flagged
// data-server-rendered) so the client hydrates the markup instead of mounting.
func (e *Engine) shell(assetHead string, dataPage []byte, nonce string, ssr SSRResult) []byte {
	// Head = app-supplied <head> (Config.Head) + the Vite/manifest asset tags +
	// any SSR head tags (title/meta/ssr <style>). Under a strict CSP, stamp a
	// per-request nonce on every <script>/<link> so they're allowed by
	// `script-src 'nonce-…'` / `style-src 'nonce-…'`.
	head := e.customHead + assetHead + strings.Join(ssr.Head, "")
	if nonce != "" {
		head = stampNonce(head, nonce)
	}

	var b strings.Builder
	b.Grow(len(dataPage) + len(head) + len(ssr.Body) + 256)
	b.WriteString("<!doctype html>\n<html>\n<head>\n")
	b.WriteString(`<meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(head)
	b.WriteString("\n</head>\n<body>\n")
	b.WriteString(`<div id="`)
	b.WriteString(e.rootView)
	// With SSR markup, flag the root so the client hydrates (Svelte keys off
	// data-server-rendered; Vue/React detect by content) and place the rendered
	// HTML inside it. ssr.Body is trusted output from the app's own SSR bundle,
	// so it is NOT escaped — unlike data-page, which always is.
	if ssr.Body != "" {
		b.WriteString(`" data-server-rendered="true" data-page="`)
		b.WriteString(html.EscapeString(string(dataPage)))
		b.WriteString(`">`)
		b.WriteString(ssr.Body)
		b.WriteString(`</div>`)
	} else {
		b.WriteString(`" data-page="`)
		b.WriteString(html.EscapeString(string(dataPage)))
		b.WriteString(`"></div>`)
	}
	b.WriteString("\n</body>\n</html>\n")
	return []byte(b.String())
}
