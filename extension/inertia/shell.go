package inertia

import (
	"html"
	"strings"
)

// shell renders the full HTML document for a non-XHR (initial) page load. The
// serialized page object is embedded in the root element's data-page attribute
// — the Inertia client adapter reads it on boot to mount the first component
// without a follow-up request. dataPage is HTML-attribute-escaped so the JSON's
// quotes and angle brackets can't break out of the attribute or inject markup.
//
// .SSRBody (server-rendered HTML inside the root div) is intentionally absent
// in this release; the slot is reserved for the SSR work and added without
// changing this signature.
func (e *Engine) shell(dataPage []byte, nonce string) []byte {
	// Head = app-supplied <head> (Config.Head) + the Vite/manifest asset tags.
	// Under a strict CSP, stamp a per-request nonce on every <script>/<link> so
	// they're allowed by `script-src 'nonce-…'` / `style-src 'nonce-…'`.
	head := e.customHead + e.head
	if nonce != "" {
		head = stampNonce(head, nonce)
	}

	var b strings.Builder
	b.Grow(len(dataPage) + len(head) + 256)
	b.WriteString("<!doctype html>\n<html>\n<head>\n")
	b.WriteString(`<meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString(head)
	b.WriteString("\n</head>\n<body>\n")
	b.WriteString(`<div id="`)
	b.WriteString(e.rootView)
	b.WriteString(`" data-page="`)
	b.WriteString(html.EscapeString(string(dataPage)))
	b.WriteString(`"></div>`)
	b.WriteString("\n</body>\n</html>\n")
	return []byte(b.String())
}

// stampNonce adds nonce="…" to every <script>/<link> tag in the head. The tags
// are engine-generated with a known shape (always "<script "/"<link " followed
// by attributes), so a targeted prefix replace is safe and avoids a parser.
func stampNonce(head, nonce string) string {
	attr := ` nonce="` + html.EscapeString(nonce) + `"`
	head = strings.ReplaceAll(head, "<script ", "<script"+attr+" ")
	head = strings.ReplaceAll(head, "<link ", "<link"+attr+" ")
	return head
}
