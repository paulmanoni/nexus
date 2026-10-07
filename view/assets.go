package view

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2/config"

	"github.com/paulmanoni/nexus/v2/httpx"
)

//go:embed runtime.js
var runtimeJS string

//go:embed behaviors.js
var behaviorsJS string

// importJS gives the runtime import(), which a classic script cannot parse
// everywhere: islands load their modules through it. Script puts it first,
// so it runs before the runtime; the event covers a page that does not.
const importJS = "window.__nxImport = function (url) { return import(url); };\n" +
	"window.dispatchEvent(new Event(\"nx:import\"));\n"

// RuntimeJS is the browser runtime, for tests that run compiled
// expressions outside a browser.
func RuntimeJS() string { return runtimeJS }

var (
	twinsMu     sync.Mutex
	twins       = map[string]string{}
	twinsScript string // twinsJS's output, "" until built after a change
	twinsVer    string
)

// RegisterTwins adds compiled expressions, keyed by id. Generated code calls
// it from init.
func RegisterTwins(m map[string]string) {
	twinsMu.Lock()
	defer twinsMu.Unlock()
	for id, js := range m {
		twins[id] = js
	}
	twinsScript, twinsVer = "", ""
}

func twinsJS() string {
	twinsMu.Lock()
	defer twinsMu.Unlock()
	return twinsLocked()
}

// twinsLocked builds the twins script once per change to the twins.
func twinsLocked() string {
	if twinsScript != "" {
		return twinsScript
	}
	ids := make([]string, 0, len(twins))
	for id := range twins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("window.__nxTwins = Object.assign(window.__nxTwins || {}, {\n")
	for _, id := range ids {
		b.WriteString(strconv.Quote(id) + ": " + twins[id] + ",\n")
	}
	b.WriteString("});\n")
	twinsScript, twinsVer = b.String(), ""
	return twinsScript
}

func version(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// twinsVersion is the twins script's version, built once per change.
func twinsVersion() string {
	twinsMu.Lock()
	defer twinsMu.Unlock()
	if twinsVer == "" {
		twinsVer = version(twinsLocked())
	}
	return twinsVer
}

// runtimeVersions are the fixed scripts' versions, hashed once.
var runtimeVersions = sync.OnceValues(func() (string, string) { return version(importJS), version(runtimeJS) })

// Script loads the browser runtime and the page's compiled expressions. Put
// it in the document head:
//
//	<head>
//	    @view.Script()
//	</head>
func Script() templ.Component {
	imp, rt := runtimeVersions()
	return templ.Raw(browserConfig() + `<style>nx-t,nx-if,nx-shard,nx-c{display:contents}nx-if[hidden]{display:none}nx-island{display:block}[data-nx-busy][aria-busy=true]{pointer-events:none}</style>` +
		`<script type="module" src="/_view/import.js?v=` + imp + `"></script>` +
		`<script src="/_view/twins.js?v=` + twinsVersion() + `" defer></script>` +
		`<script src="/_view/runtime.js?v=` + rt + `" defer></script>`)
}

// Behaviors loads behaviors.js: what a page's markup asks for with data-nx
// attributes and no styling — a box filtering a list, a "select all" box,
// a form's submit enabled once it is valid. Put it after Script:
//
//	@view.Script()
//	@view.Behaviors()
func Behaviors() templ.Component {
	return templ.Raw(`<script src="/_view/behaviors.js?v=` + behaviorsVersion() + `" defer></script>`)
}

var behaviorsVersion = sync.OnceValue(func() string { return version(behaviorsJS) })

func serveJS(body func() string) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		c.Header("Cache-Control", "no-cache")
		c.Data(200, "text/javascript; charset=utf-8", []byte(body()))
	}
}

// browserConfig is the configuration [runtime.browser] config sends to the
// browser, as the JSON __nx.config reads; "" when it sends none. json.Marshal
// escapes <, > and &, so a value can't end the script element.
func browserConfig() string {
	values, err := config.BrowserValues()
	if err != nil || len(values) == 0 {
		return ""
	}
	b, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return `<script type="application/json" id="nx-config">` + string(b) + `</script>`
}
