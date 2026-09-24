package inertia

import (
	"bytes"
	"html"
	"net/url"
	"sort"
	"strings"
)

// devReloadScript is the framework's live-reload shim, mounted by
// ServeFrontend under nexus dev.
const devReloadScript = "/__nexus/dev/script.js"

// pageTemplate is the app's index.html (App.FrontendDocument) located for a
// render: where the page goes and where head tags go. Everything else in the
// document is written back byte for byte.
type pageTemplate struct {
	doc        []byte
	mount      htmlTag // the start tag whose id is Config.RootView
	mountClose int     // offset of the mount's matching end tag; -1 if none
	headClose  int     // offset of </head>; -1 if none
	bodyOpen   int     // offset of <body>; -1 if none
	hasShim    bool    // the document already loads devReloadScript
	unnonced   []int   // tag-name ends of <script>/<link>/<style> without a nonce
	fromDev    bool    // FrontendDocument.FromDevServer
}

// parseTemplate locates the mount element (the first element whose id is
// rootID) and the head close in doc. Tags are found by scanTags, so an id in
// a comment, a script, or another attribute's value is never mistaken for
// the mount. ok is false when the document has no such element.
func parseTemplate(doc []byte, rootID string) (t pageTemplate, ok bool) {
	t = pageTemplate{doc: doc, mountClose: -1, headClose: -1, bodyOpen: -1}
	depth := 0
	scanTags(doc, func(tag htmlTag) bool {
		switch {
		case !ok && !tag.end:
			if id, has := tag.attr("id"); has && id.value == rootID {
				t.mount, ok = tag, true
				if !voidElems[tag.name] {
					depth = 1
				}
			}
		case ok && depth > 0 && tag.name == t.mount.name:
			if tag.end {
				if depth--; depth == 0 {
					t.mountClose = tag.start
				}
			} else {
				depth++ // <div/> opens a div in HTML
			}
		}
		if tag.end {
			if tag.name == "head" && t.headClose < 0 {
				t.headClose = tag.start
			}
			return true
		}
		switch tag.name {
		case "body":
			if t.bodyOpen < 0 {
				t.bodyOpen = tag.start
			}
		case "script", "link", "style":
			if _, has := tag.attr("nonce"); !has {
				t.unnonced = append(t.unnonced, tag.nameEnd)
			}
			if tag.name == "script" {
				if src, has := tag.attr("src"); has && isDevReloadSrc(src.value) {
					t.hasShim = true
				}
			}
		}
		return true
	})
	return t, ok
}

// isDevReloadSrc reports whether src loads the shim from the page's own
// origin. A copy made absolute against another origin (the Vite dev server
// rewrites root-relative URLs in its index.html) does not count.
func isDevReloadSrc(src string) bool {
	u, err := url.Parse(src)
	return err == nil && u.Host == "" && u.Path == devReloadScript
}

// render writes the document with the page in it:
//
//   - the mount element gets data-page (replacing one the template has), and
//     data-server-rendered with its content replaced by the SSR body when
//     there is one — a template's loader markup stays for client rendering;
//   - head (Config.Head, the reload shim, SSR head tags) goes before </head>;
//   - with a nonce, every <script>/<link>/<style> without one gets it — the
//     template's own tags included, so a strict CSP that held with the
//     engine's manifest tags still holds with index.html's.
//
// dataPage is escaped for the attribute exactly as the synthesised shell
// does. head is inserted verbatim (already nonce-stamped by the caller).
func (t pageTemplate) render(dataPage []byte, nonce string, ssr SSRResult, head string) []byte {
	type edit struct {
		at, to int
		s      string
	}
	var edits []edit
	if nonce != "" {
		attr := nonceAttr(nonce)
		for _, at := range t.unnonced {
			edits = append(edits, edit{at, at, attr})
		}
	}
	for _, a := range t.mount.attrs {
		if a.name == "data-page" || a.name == "data-server-rendered" {
			edits = append(edits, edit{a.start, a.stop, ""})
		}
	}
	var mountAttrs strings.Builder
	if ssr.Body != "" {
		mountAttrs.WriteString(` data-server-rendered="true"`)
	}
	mountAttrs.WriteString(` data-page="`)
	mountAttrs.WriteString(html.EscapeString(string(dataPage)))
	mountAttrs.WriteString(`"`)
	edits = append(edits, edit{t.mount.closeAt, t.mount.closeAt, mountAttrs.String()})
	if ssr.Body != "" {
		to := t.mount.stop
		if t.mountClose >= 0 {
			to = t.mountClose
		}
		edits = append(edits, edit{t.mount.stop, to, ssr.Body})
	}
	if head != "" {
		at := t.headClose
		if at < 0 {
			at = t.bodyOpen
		}
		if at < 0 {
			at = t.mount.start
		}
		edits = append(edits, edit{at, at, head})
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].at < edits[j].at })

	var b bytes.Buffer
	b.Grow(len(t.doc) + len(dataPage) + len(head) + len(ssr.Body) + 64)
	pos := 0
	for _, e := range edits {
		if e.at < pos {
			// Inside a range an earlier edit replaced: the SSR body took the
			// mount's content, and with it any template tag a nonce was due on.
			continue
		}
		b.Write(t.doc[pos:e.at])
		b.WriteString(e.s)
		pos = e.to
	}
	b.Write(t.doc[pos:])
	return b.Bytes()
}

func nonceAttr(nonce string) string {
	return ` nonce="` + html.EscapeString(nonce) + `"`
}

// stampNonce adds nonce="…" to every <script>, <link> and <style> in an
// engine-injected fragment that lacks one, so it satisfies a strict
// `script-src 'nonce-…'` / `style-src 'nonce-…'` policy.
func stampNonce(fragment, nonce string) string {
	if nonce == "" || fragment == "" {
		return fragment
	}
	doc := []byte(fragment)
	var at []int
	scanTags(doc, func(tag htmlTag) bool {
		if !tag.end && (tag.name == "script" || tag.name == "link" || tag.name == "style") {
			if _, has := tag.attr("nonce"); !has {
				at = append(at, tag.nameEnd)
			}
		}
		return true
	})
	if len(at) == 0 {
		return fragment
	}
	attr := nonceAttr(nonce)
	var b strings.Builder
	b.Grow(len(fragment) + len(at)*len(attr))
	pos := 0
	for _, i := range at {
		b.WriteString(fragment[pos:i])
		b.WriteString(attr)
		pos = i
	}
	b.WriteString(fragment[pos:])
	return b.String()
}

// loadsModule reports whether fragment has a <script type="module"> — an app
// that loads its client itself through Config.Head.
func loadsModule(fragment string) bool {
	found := false
	scanTags([]byte(fragment), func(tag htmlTag) bool {
		if !tag.end && tag.name == "script" {
			if ty, ok := tag.attr("type"); ok && strings.EqualFold(strings.TrimSpace(ty.value), "module") {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// htmlTag is a start or end tag found by scanTags. Offsets index the scanned
// document.
type htmlTag struct {
	name        string // lowercased
	end         bool   // </name>
	start, stop int    // the tag's span: '<' up to and including '>'
	nameEnd     int    // just after the tag name: where an attribute can go
	closeAt     int    // where an appended attribute goes: the '>' or the '/' of '/>'
	selfClosing bool   // written as <name … />
	attrs       []htmlAttr
}

// htmlAttr is one attribute of a tag. Its span starts at the whitespace
// before the name, so removing [start, stop) leaves the tag well formed.
type htmlAttr struct {
	name        string // lowercased
	value       string // entity-decoded
	start, stop int
}

// attr returns the first attribute called name, which is the one browsers use.
func (t htmlTag) attr(name string) (htmlAttr, bool) {
	for _, a := range t.attrs {
		if a.name == name {
			return a, true
		}
	}
	return htmlAttr{}, false
}

// rawTextElems hold text a browser never parses as markup: a tag-shaped
// string inside a script, style, or title is not a tag.
var rawTextElems = map[string]bool{
	"script": true, "style": true, "textarea": true, "title": true, "xmp": true,
	"iframe": true, "noembed": true, "noframes": true, "noscript": true,
}

var voidElems = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "source": true,
	"track": true, "wbr": true,
}

// scanTags calls fn for each start and end tag of doc in document order until
// fn returns false. It is the HTML tokenizer's tag-finding subset: comments,
// doctypes and processing instructions are skipped, and so is the content of
// raw-text elements (script, style, title, …) up to their end tag, so markup
// that only appears inside those — or inside a quoted attribute value — is
// never reported. Text is not reported. A tag left unterminated at the end of
// the document ends the scan.
func scanTags(doc []byte, fn func(htmlTag) bool) {
	n := len(doc)
	for i := 0; i < n; {
		lt := bytes.IndexByte(doc[i:], '<')
		if lt < 0 {
			return
		}
		i += lt
		switch {
		case bytes.HasPrefix(doc[i:], []byte("<!--")):
			// Searching from "<!" also closes the empty forms <!--> and <!--->.
			end := bytes.Index(doc[i+2:], []byte("-->"))
			if end < 0 {
				return
			}
			i += 2 + end + 3
		case i+1 < n && (doc[i+1] == '!' || doc[i+1] == '?'),
			i+1 < n && doc[i+1] == '/' && (i+2 >= n || !isASCIILetter(doc[i+2])):
			// Doctype, CDATA, processing instruction, or a bogus end tag:
			// all run to the next '>'.
			end := bytes.IndexByte(doc[i:], '>')
			if end < 0 {
				return
			}
			i += end + 1
		case i+1 < n && (isASCIILetter(doc[i+1]) || doc[i+1] == '/'):
			tag, ok := parseTag(doc, i)
			if !ok || !fn(tag) {
				return
			}
			i = tag.stop
			if !tag.end && rawTextElems[tag.name] {
				i = rawTextEnd(doc, i, tag.name)
			}
			if !tag.end && tag.name == "plaintext" {
				return
			}
		default:
			i++
		}
	}
}

// parseTag parses the tag starting at doc[i] ('<' followed by a letter, or
// "</" followed by a letter).
func parseTag(doc []byte, i int) (htmlTag, bool) {
	n := len(doc)
	t := htmlTag{start: i}
	j := i + 1
	if doc[j] == '/' {
		t.end = true
		j++
	}
	ns := j
	for j < n && !isHTMLSpace(doc[j]) && doc[j] != '/' && doc[j] != '>' {
		j++
	}
	t.name = strings.ToLower(string(doc[ns:j]))
	t.nameEnd = j
	for {
		ws := j
		slash := -1
		for j < n && (isHTMLSpace(doc[j]) || doc[j] == '/') {
			if doc[j] == '/' {
				slash = j
			}
			j++
		}
		if j >= n {
			return t, false
		}
		if doc[j] == '>' {
			t.stop = j + 1
			t.closeAt = j
			if slash == j-1 {
				t.selfClosing = true
				t.closeAt = slash
			}
			return t, true
		}
		as := j
		j++ // a name may begin with '='
		for j < n && !isHTMLSpace(doc[j]) && doc[j] != '/' && doc[j] != '>' && doc[j] != '=' {
			j++
		}
		a := htmlAttr{name: strings.ToLower(string(doc[as:j])), start: ws}
		k := j
		for k < n && isHTMLSpace(doc[k]) {
			k++
		}
		if k < n && doc[k] == '=' {
			k++
			for k < n && isHTMLSpace(doc[k]) {
				k++
			}
			switch {
			case k < n && (doc[k] == '"' || doc[k] == '\''):
				end := bytes.IndexByte(doc[k+1:], doc[k])
				if end < 0 {
					return t, false
				}
				a.value = html.UnescapeString(string(doc[k+1 : k+1+end]))
				j = k + 1 + end + 1
			default:
				vs := k
				for k < n && !isHTMLSpace(doc[k]) && doc[k] != '>' {
					k++
				}
				a.value = html.UnescapeString(string(doc[vs:k]))
				j = k
			}
		}
		a.stop = j
		t.attrs = append(t.attrs, a)
	}
}

// rawTextEnd returns the offset of the end tag closing a raw-text element
// whose content starts at i, or len(doc) when it is never closed.
func rawTextEnd(doc []byte, i int, name string) int {
	for {
		k := bytes.Index(doc[i:], []byte("</"))
		if k < 0 {
			return len(doc)
		}
		i += k
		e := i + 2 + len(name)
		if e <= len(doc) && strings.EqualFold(string(doc[i+2:e]), name) &&
			(e == len(doc) || isHTMLSpace(doc[e]) || doc[e] == '/' || doc[e] == '>') {
			return i
		}
		i += 2
	}
}

func isASCIILetter(c byte) bool { return c|0x20 >= 'a' && c|0x20 <= 'z' }

func isHTMLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}
