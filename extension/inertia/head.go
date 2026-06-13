package inertia

import (
	"html"
	"strings"
)

// Head is the typed document <head> the Inertia shell renders on full-page
// loads: the app title, meta tags, and stylesheet/font links that index.html
// would normally carry. The engine adds charset/viewport and the Vite/manifest
// asset tags itself, so list only the app-specific extras here.
//
//	inertia.Head{
//	    Title: "My App",
//	    Meta:  []inertia.Meta{{Name: "description", Content: "..."}},
//	    Links: []inertia.Link{
//	        {Rel: "stylesheet", Href: "/icons/font.css"},
//	        {Rel: "preconnect", Href: "https://fonts.gstatic.com", CrossOrigin: true},
//	    },
//	}
type Head struct {
	Title string // <title>…</title>
	Meta  []Meta // <meta …>
	Links []Link // <link …>
	Raw   string // appended verbatim — escape hatch for anything not modeled
}

// Meta is a <meta> tag. Set Name OR Property (HTTP-equiv-style tags go in Raw).
type Meta struct {
	Name     string // name="…"
	Property string // property="…" (Open Graph etc.)
	Content  string // content="…"
}

// Link is a <link> tag (stylesheet, preconnect, icon, preload, …).
type Link struct {
	Rel         string
	Href        string
	Type        string
	As          string
	CrossOrigin bool // emits a bare `crossorigin` attribute
}

// empty reports whether the Head contributes nothing.
func (h Head) empty() bool {
	return h.Title == "" && len(h.Meta) == 0 && len(h.Links) == 0 && h.Raw == ""
}

// render serializes the Head to HTML for injection into the shell's <head>.
// Attribute values are HTML-escaped; Raw is emitted verbatim (caller-trusted).
func (h Head) render() string {
	if h.empty() {
		return ""
	}
	var b strings.Builder
	if h.Title != "" {
		b.WriteString("<title>")
		b.WriteString(html.EscapeString(h.Title))
		b.WriteString("</title>")
	}
	for _, m := range h.Meta {
		b.WriteString("<meta")
		attr(&b, "name", m.Name)
		attr(&b, "property", m.Property)
		attr(&b, "content", m.Content)
		b.WriteString(">")
	}
	for _, l := range h.Links {
		b.WriteString("<link")
		attr(&b, "rel", l.Rel)
		attr(&b, "href", l.Href)
		attr(&b, "type", l.Type)
		attr(&b, "as", l.As)
		if l.CrossOrigin {
			b.WriteString(" crossorigin")
		}
		b.WriteString(">")
	}
	b.WriteString(h.Raw)
	return b.String()
}

// attr writes ` key="escaped"` when v is non-empty.
func attr(b *strings.Builder, key, v string) {
	if v == "" {
		return
	}
	b.WriteString(" ")
	b.WriteString(key)
	b.WriteString(`="`)
	b.WriteString(html.EscapeString(v))
	b.WriteString(`"`)
}
