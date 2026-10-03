package view

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/a-h/templ"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/frontend/vitehot"
	"github.com/paulmanoni/nexus/v2/frontend/vitemanifest"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// IslandOption says when an island mounts, and whether the server renders it.
type IslandOption func(*island)

type island struct {
	when string
	ssr  bool
}

// Idle mounts the island once the browser is idle.
func Idle() IslandOption { return func(i *island) { i.when = "idle" } }

// Visible mounts the island when it is about to scroll into view.
func Visible() IslandOption { return func(i *island) { i.when = "visible" } }

// Media mounts the island once the media query matches:
//
//	view.Media("(min-width: 768px)")
func Media(query string) IslandOption { return func(i *island) { i.when = "media:" + query } }

// SSR renders the island on the server too, so its HTML is in the page
// before any JavaScript runs (search engines, slow phones); the browser then
// hydrates it. The server is the bundle nexus build writes when
// vite.config has nexus({ islands: { ssr: true } }) — run it beside the
// binary with node web/dist/ssr/islands.js. Under nexus dev, and whenever
// that server does not answer, the island renders in the browser only.
func SSR() IslandOption { return func(i *island) { i.ssr = true } }

var islandName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_\-]*(/[A-Za-z0-9_][A-Za-z0-9_\-]*)*$`)

var (
	declaredMu sync.Mutex
	declared   = map[string]reflect.Type{} // island name -> props type
)

// NewIsland declares an island: a component of the project's Vite frontend
// that templ pages mount — a Vue (.vue) or React (.tsx, .jsx) component, or
// a module exporting mount(el, props), under web/src/islands, named by its
// path there without the extension. Declare it once, at package level:
//
//	var Chart = view.NewIsland[ChartProps]("Chart")
//
// and place it like a component; its children are rendered on the server
// and stay until the island mounts:
//
//	@Chart(ChartProps{Points: pts}, view.Visible()) {
//	    <div class="h-64 animate-pulse"></div>
//	}
//
// P must encode to a JSON object; the client SDK types it for the component
// as NexusIslandProps["Chart"]. Its *view.Signal fields stay live in the
// browser: the island gets the value, and is updated when the signal
// changes. A Vue island sets one back with emit('update:field', v)
// (defineModel), a React island with props.setField(v).
//
// An island mounts as soon as the page loads, or later with Idle, Visible or
// Media, and SSR renders it on the server too. On a live page a re-render
// with new props updates the mounted island rather than remounting it, and
// in-app navigation unmounts islands that leave the page.
//
// The loader comes from the Vite dev server while one runs (nexus dev), and
// from the build's manifest otherwise; the frontend is the one
// nexus.ServeFrontend serves. Without one — no frontend, or a build without
// this island — the children stay, the reason is logged once and set as the
// element's data-error, and the page still renders: in a Go test, say.
func NewIsland[P any](name string) func(props P, opts ...IslandOption) templ.Component {
	if !islandName.MatchString(name) {
		panic(fmt.Sprintf("view.NewIsland(%q): an island is named by its path under src/islands, without the extension", name))
	}
	t := reflect.TypeFor[P]()
	declaredMu.Lock()
	if prev, ok := declared[name]; ok && prev != t {
		declaredMu.Unlock()
		panic(fmt.Sprintf("view.NewIsland(%q): declared twice, with %s and %s", name, prev, t))
	}
	declared[name] = t
	declaredMu.Unlock()
	return func(props P, opts ...IslandOption) templ.Component {
		return renderIsland(name, props, opts)
	}
}

// registerIslands records every declared island's props type with the app,
// for the client SDK.
func registerIslands(app *nexus.App) {
	declaredMu.Lock()
	defer declaredMu.Unlock()
	for name, t := range declared {
		app.RegisterIsland(name, t)
	}
}

func renderIsland(name string, props any, opts []IslandOption) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var cfg island
		for _, o := range opts {
			o(&cfg)
		}
		raw, err := json.Marshal(props)
		if err != nil {
			return fmt.Errorf("island %s: props: %w", name, err)
		}
		if bytes.Equal(raw, []byte("null")) {
			raw = []byte("{}")
		}
		if len(raw) == 0 || raw[0] != '{' {
			return fmt.Errorf("island %s: props must encode to a JSON object, got %s", name, raw)
		}
		app := appFrom(ctx)
		src, err := islandsSource(app, name)
		var b strings.Builder
		b.WriteString(`<nx-island data-c="` + html.EscapeString(name) + `"`)
		if err != nil {
			islandWarn.Do(func() { log.Printf("nexus view: islands are not loading: %v", err) })
			b.WriteString(` data-error="` + html.EscapeString(err.Error()) + `"`)
		} else {
			b.WriteString(` data-l="` + html.EscapeString(src.loader) + `"`)
		}
		b.WriteString(` data-p="` + html.EscapeString(string(raw)) + `"`)
		if cfg.when != "" {
			b.WriteString(` data-when="` + html.EscapeString(cfg.when) + `"`)
		}
		var rendered string
		// A live page's socket re-render keeps the mounted island (the
		// morph leaves its DOM alone), so it is not rendered again.
		if err == nil && cfg.ssr && !src.dev && ctx.Value(socketKey{}) == nil {
			rendered, err = ssrIsland(ctx, name, raw)
			if err != nil {
				log.Printf("nexus view: island %s renders in the browser only: %v", name, err)
			} else {
				b.WriteString(" data-ssr")
			}
		}
		b.WriteString(">")
		if _, err := io.WriteString(w, b.String()); err != nil {
			return err
		}
		if rendered != "" {
			_, err = io.WriteString(w, rendered)
		} else if children := templ.GetChildren(ctx); children != nil {
			err = children.Render(templ.ClearChildren(ctx), w)
		}
		if err != nil {
			return err
		}
		_, err = io.WriteString(w, "</nx-island>")
		return err
	})
}

type (
	appKey    struct{}
	socketKey struct{} // a render for a live page's socket
)

// lastApp is the most recently booted app, for a render that did not come
// through a request (a component rendered directly).
var lastApp atomic.Pointer[nexus.App]

func withApp(ctx context.Context, c *httpx.Ctx) context.Context {
	if app, ok := nexus.AppFromGin(c); ok {
		return context.WithValue(ctx, appKey{}, app)
	}
	return ctx
}

func appFrom(ctx context.Context) *nexus.App {
	if app, ok := ctx.Value(appKey{}).(*nexus.App); ok {
		return app
	}
	return lastApp.Load()
}

// source is where a page loads its islands from.
type source struct {
	loader string // the loader module's URL
	dev    bool   // the Vite dev server
}

// islandsSource finds the islands loader: the dev server's while one
// serves the project's islands, else the build's — which must have name.
func islandsSource(app *nexus.App, name string) (source, error) {
	if app == nil {
		return source{}, errors.New("no nexus app is serving this render")
	}
	if r := app.ViteHot(); r != nil {
		if h, err := r.Current(); err == nil && h != nil && h.Islands {
			return source{loader: h.URL("@id/" + vitehot.IslandsModule), dev: true}, nil
		}
	}
	fsys, root, ok := app.FrontendFS()
	if !ok {
		return source{}, errors.New("no frontend: islands are built by the Vite project nexus.ServeFrontend serves")
	}
	b, err := islandsBuild(app, fsys, root)
	if err != nil {
		return source{}, err
	}
	if !b.names[name] {
		return source{}, fmt.Errorf("the frontend build has no island %s (src/islands/%s.vue, .tsx, …) — rebuild it", name, name)
	}
	return source{loader: strings.TrimRight(app.FrontendMount(), "/") + "/" + strings.TrimPrefix(b.loader, "/")}, nil
}

var islandWarn sync.Once

// build is what a frontend build holds for islands.
type build struct {
	loader string          // the loader's file
	names  map[string]bool // the islands it has
}

var islandsBuilds sync.Map // *nexus.App -> build, a production build's

// islandsBuild reads the islands from the build manifest. Production caches
// it; development reads again, as a build can land while the app runs.
func islandsBuild(app *nexus.App, fsys fs.FS, root string) (build, error) {
	dev := vitehot.Enabled(dev.Enabled(), app.Environment())
	if !dev {
		if b, ok := islandsBuilds.Load(app); ok {
			return b.(build), nil
		}
	}
	m, err := vitemanifest.Load(fsys, root)
	if err != nil {
		if dev {
			return build{}, errors.New("no Vite dev server is serving islands and the frontend is not built — run nexus dev, or nexus build")
		}
		return build{}, err
	}
	b := build{loader: m.IslandsFile(), names: map[string]bool{}}
	if b.loader == "" {
		return build{}, fmt.Errorf("%s has no islands loader — is there a src/islands directory beside nexus() in vite.config?", m.Path)
	}
	for key := range m.Chunks {
		if n, ok := strings.CutPrefix(key, vitemanifest.IslandPrefix); ok {
			b.names[n] = true
		}
	}
	if !dev {
		islandsBuilds.Store(app, b)
	}
	return b, nil
}

// DefaultIslandServer is where SSR islands render unless IslandServer says
// otherwise: the port node web/dist/ssr/islands.js listens on by default.
const DefaultIslandServer = "http://127.0.0.1:13715"

var islandServer atomic.Pointer[string]

// IslandServer sets the address of the islands server SSR islands render
// on (DefaultIslandServer when unset) — for one on another port or host:
//
//	nexus.Boot(view.IslandServer("http://127.0.0.1:14000"))
//
// The server reads its port from NEXUS_ISLANDS_PORT.
func IslandServer(url string) nexus.Option {
	return nexus.Invoke(func() { islandServer.Store(&url) })
}

var ssrClient = &http.Client{Timeout: 2 * time.Second}

// ssrIsland renders an island on the islands server. Its props are the
// values the browser starts from: signals become their values.
func ssrIsland(ctx context.Context, name string, raw []byte) (string, error) {
	var props any
	if err := json.Unmarshal(raw, &props); err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{"name": name, "props": signalValues(props)})
	if err != nil {
		return "", err
	}
	base := DefaultIslandServer
	if u := islandServer.Load(); u != nil && *u != "" {
		base = *u
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/render", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := ssrClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		HTML  string `json:"html"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("islands server: %w", err)
	}
	if res.StatusCode != http.StatusOK || out.Error != "" {
		return "", fmt.Errorf("islands server: HTTP %d: %s", res.StatusCode, out.Error)
	}
	return out.HTML, nil
}

// signalValues replaces every {"$sig": id, "v": value} in v with its value.
func signalValues(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["$sig"].(string); ok {
			return signalValues(t["v"])
		}
		for k, e := range t {
			t[k] = signalValues(e)
		}
	case []any:
		for i, e := range t {
			t[i] = signalValues(e)
		}
	}
	return v
}
