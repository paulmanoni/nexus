// Package ui is a small component kit for nexus views: buttons, form
// fields, tabs, dialogs, menus, data tables, toasts, loaders, page headers
// and badges, written in templ and styled with Tailwind utility classes
// over CSS variables (light and dark).
//
//	<head>
//	    @view.Script()
//	    @ui.Script()
//	</head>
//
//	@ui.Button(ui.ButtonProps{Variant: ui.Primary, OnClick: view.Send(x.Save), Hotkey: "mod+s"}) {
//	    Save
//	}
//
// Importing the package serves its two files — /_view/ui/ui.js (the
// browser behaviour: loader overlays, toasts, hotkeys, copy-link, menus,
// dialogs, tabs and row-click navigation) and /_view/ui/ui.css (the theme
// tokens and the few rules utilities can't express) — and Script loads
// them. The components' classes are Tailwind utilities: nexus dev and
// nexus build add this package's templates to the app's Tailwind sources.
//
// Apps that want to own a component copy it with `nexus add ui <name>`.
package ui

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"strings"
	"sync"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/view"
)

//go:embed ui.js
var uiJS string

//go:embed ui.css
var uiCSS string

// JS is the kit's browser behaviour (ui.js), for tests and vendoring.
func JS() string { return uiJS }

// CSS is the kit's stylesheet (ui.css): theme tokens and loader rules.
func CSS() string { return uiCSS }

// Prefix is where the kit's files are served.
const Prefix = "/_view/ui/"

// Importing the package serves ui.js and ui.css under Prefix.
func init() {
	nexus.RegisterBuiltinOptions(func() []nexus.Option {
		return []nexus.Option{view.Assets(Prefix, http.HandlerFunc(serve))}
	})
}

func serve(w http.ResponseWriter, r *http.Request) {
	var body, typ string
	switch strings.TrimPrefix(r.URL.Path, Prefix) {
	case "ui.js":
		body, typ = uiJS, "text/javascript; charset=utf-8"
	case "ui.css":
		body, typ = uiCSS, "text/css; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(body))
}

func version(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// Script loads the kit's stylesheet and behaviour. Put it in the document
// head, after view.Script():
//
//	@view.Script()
//	@ui.Script()
func Script() templ.Component {
	css, js := versions()
	return templ.Join(
		templ.Raw(`<link rel="stylesheet" href="`+Prefix+`ui.css?v=`+css+`"/>`+
			`<script src="`+Prefix+`ui.js?v=`+js+`" defer></script>`),
		view.Behaviors(),
	)
}

// versions are the kit's files' versions, hashed once.
var versions = sync.OnceValues(func() (string, string) { return version(uiCSS), version(uiJS) })

// Loading shows a loader over the elements with the given ids while the
// event works: it covers them as the click (or submit) happens and lifts
// when the live page's reply arrives. With no ids it covers the live page
// region that sends the event.
//
//	<button onclick={ ui.Loading(view.Send(x.Run), "preview") }>Run</button>
//
// It wraps any script — view.Send, view.Submit, view.Change — and is the
// kit's spelling of the design's view.Send(x.Run).Loading("preview"). For
// markup that can't wrap the script (a templ.Attributes map), the attribute
// form data-ui-loading="preview" does the same on click; see LoadingAttr.
func Loading(script templ.ComponentScript, ids ...string) templ.ComponentScript {
	if ids == nil {
		ids = []string{}
	}
	b, _ := json.Marshal(ids)
	pre := html.EscapeString("window.nxui&&nxui.loading(this," + string(b) + ");")
	script.Call = pre + script.Call
	return script
}

// LoadingAttr is the attribute form of Loading, for a templ.Attributes map
// or a spread: the click shows the loader over the elements with the
// given ids (space-separated in the attribute).
//
//	<button onclick={ view.Send(x.Run) } { ui.LoadingAttr("preview")... }>Run</button>
func LoadingAttr(ids ...string) templ.Attributes {
	return templ.Attributes{"data-ui-loading": strings.Join(ids, " ")}
}

// CopyLink makes an element copy a link when clicked: a path (/orders/7)
// becomes an absolute URL on the page's origin; anything else is copied as
// is. A toast confirms.
//
//	<button { ui.CopyLink("/orders/7")... }>Copy link</button>
func CopyLink(pathOrText string) templ.Attributes {
	return templ.Attributes{"data-ui-copy": pathOrText}
}

// Hotkey makes a key combination click the element: "mod+s" (Ctrl or
// Cmd), "mod+enter", "shift+?", "/", "escape". A disabled element is
// skipped; a combination typed into a field fires only with a modifier.
func Hotkey(combo string) templ.Attributes {
	return templ.Attributes{"data-ui-hotkey": combo}
}

// OpenDialog opens the browser-side Dialog with id when clicked.
func OpenDialog(id string) templ.Attributes {
	return templ.Attributes{"data-ui-open": id}
}

// CloseDialog closes the browser-side Dialog the element is in.
func CloseDialog() templ.Attributes {
	return templ.Attributes{"data-ui-close": true}
}
