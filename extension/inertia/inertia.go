// Package inertia adds Inertia.js (https://inertiajs.com) support to nexus:
// server-driven pages that return a typed props struct instead of building a
// client-side API. A page handler stays an ordinary nexus reflective handler;
// the engine wraps its return into the Inertia page protocol — a JSON page
// object for XHR visits, a full HTML document for initial loads — reusing
// nexus's params binding, validation, DI, auth gates, tracing, and metrics.
//
// Wire it as a module alongside the static-asset serving that ServeFrontend
// provides for the built bundle:
//
//	//go:embed all:web/dist
//	var webFS embed.FS
//
//	nexus.Boot(
//	    nexus.ServeFrontend(webFS, "web/dist"),  // serves assets; names the bundle
//	    inertia.Module(inertia.Config{}),        // the page protocol — bundle auto-discovered
//	    inertia.Share(SharedAuth),
//	    inertia.Page("GET", "/users", "Users/Index", NewListUsers),
//	)
package inertia

import (
	"io/fs"
	"log"
	"sync"

	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/appctx"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/extension"
)

// AutoVersion is the zero value for Config.Version: derive the Inertia asset
// version from a hash of the build manifest. Set Config.Version to a fixed
// string to pin it instead.
const AutoVersion = ""

// engineKeyT keys the per-app Inertia engine in App.SetValue/Value. A private
// type avoids any collision with other extensions' keys.
type engineKeyT struct{}

// Config configures the Inertia engine.
type Config struct {
	// Frontend is the embedded filesystem holding the built bundle, read for
	// the Vite manifest (production asset tags + auto version). OPTIONAL: when
	// nil, the engine auto-discovers the bundle registered by
	// nexus.ServeFrontend, so the app names its frontend once. Set this only to
	// read the manifest from a DIFFERENT source than ServeFrontend serves.
	Frontend fs.FS
	// Root is the path within Frontend to the build output (e.g. "web/dist").
	// The manifest is read from Root/.vite/manifest.json. Ignored when Frontend
	// is nil (the discovered ServeFrontend root is used instead).
	Root string
	// RootView is the id of the root element the Inertia client mounts on.
	// Defaults to "app". When pages render into index.html, the engine puts
	// the page on the element with this id (its data-page attribute); a
	// document without one is an error page in development.
	RootView string
	// Version is the Inertia asset version. Empty (AutoVersion) derives it
	// from the manifest hash; a fixed string pins it.
	Version string
	// Head is added to the <head> of every full-page load. Pages render into
	// the app's index.html (nexus.ServeFrontend's — built, or the Vite dev
	// server's), which already carries its title, meta, stylesheets and
	// asset tags, so Head is only for what index.html can't say. When there
	// is no index.html (a module-only build, nexus({ input })), the engine
	// synthesises the document: charset/viewport and the asset tags are
	// added automatically, and Head is the rest of it. A <script
	// type="module"> here counts as loading the client, so a page renders
	// without a build manifest. See the Head type for a Raw escape hatch.
	Head Head
	// EncryptHistory turns on Inertia history-state encryption for every page
	// by default (Inertia v2). A handler can override per-response with
	// inertia.EncryptHistory(c, false); inertia.ClearHistory(c) drops any
	// previously-encrypted entry (e.g. on logout).
	EncryptHistory bool
	// Entry is the dev-server module the shell loads in dev when the dev
	// server doesn't declare one. nexus-vite-plugin's hot file names the entry
	// itself, so this only matters for a hot file without one. Defaults to
	// "src/main.ts"; set "src/main.tsx" for a React app. Ignored in production, where the entry comes from the
	// build manifest.
	Entry string
	// React emits the Vite React Fast Refresh preamble before the dev client so
	// HMR works for React apps. Auto-enabled when the dev entry ends in
	// .tsx/.jsx; set it explicitly to force the preamble for a .ts/.js React
	// entry.
	React bool
	// Nonce returns the per-request CSP nonce for the document shell. When set
	// and non-empty, the engine stamps nonce="…" on every <script>/<link> it
	// injects (asset tags + dev preamble + Config.Head), so they satisfy a
	// strict `script-src 'nonce-…'` / `style-src 'nonce-…'` policy. The app's
	// CSP middleware owns generating the nonce + setting the Content-Security-
	// Policy header; return that same value here. Leave nil for no CSP nonce.
	Nonce func(*httpx.Ctx) string
	// SSR enables server-side rendering: on the initial (non-XHR) load the engine
	// POSTs the page object to this renderer and injects the returned head/body
	// into the shell, which the client then hydrates. Nil = client-only (the
	// default). Use extension/inertia/ssrhttp for the standard @inertiajs/server
	// setup. A renderer error falls back to client rendering (see SSRStrict).
	SSR SSRRenderer
	// OnSSRError is called when SSR rendering fails (transport error, bad
	// response). The engine still falls back to client rendering — this is for
	// logging/metrics, mirroring Inertia's SsrRenderFailed event. Optional.
	OnSSRError func(error)
	// SSRStrict makes an SSR failure return the error (→ 500) instead of falling
	// back to client rendering. Off by default; useful in tests/CI to catch a
	// broken SSR pipeline, like Inertia's throw_on_error.
	SSRStrict bool
	// ErrorPage is the component rendered when a page handler returns an
	// error nothing else claims (not a Redirect/Location, not validation
	// errors). A GET or HEAD visit renders it with ErrorProps{Status,
	// Message} and the error's status code — the status is the one the REST
	// error path would use (404/409/400 for the CRUD sentinels, else 500) —
	// so the browser shows the app's own page instead of Inertia's "invalid
	// response" modal over raw JSON. Any other method (a form submit)
	// redirects back with the message flashed under errors._global, which
	// useForm already reads. The error is still recorded on the request's
	// trace. Empty (the default) keeps the plain JSON error response.
	ErrorPage string
}

// ErrorProps are the props Config.ErrorPage receives. Shared props (the
// app's auth user, csrf token…) are included as on any page.
type ErrorProps struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
}

// Engine renders Inertia responses for an app. One is built per app via Module
// and shared across requests. The asset head/version are resolved at render
// time (see assets) so the bundle ServeFrontend registers is visible regardless
// of option ordering, and a dev server that restarts on another port is
// followed without a Go restart.
type Engine struct {
	rootView       string
	customHead     string // app-supplied <head> HTML (Config.Head)
	shared         []SharedProvider
	encryptHistory bool // app-wide default for page.encryptHistory

	// Frontend-resolution inputs. cfgFrontend/cfgRoot come from Config; when
	// they're empty the engine auto-discovers the bundle ServeFrontend mounted
	// via app.FrontendFS(), so the app names its frontend in one place.
	app         *nexus.App
	cfgFrontend fs.FS
	cfgRoot     string
	versionPin  string                  // Config.Version; AutoVersion ("") = derive from manifest
	devEntry    string                  // dev-server entry module (Config.Entry)
	nonceFn     func(*httpx.Ctx) string // per-request CSP nonce (Config.Nonce)
	ssr         SSRRenderer             // server-side renderer (Config.SSR); nil = client-only
	onSSRError  func(error)             // Config.OnSSRError
	ssrStrict   bool                    // Config.SSRStrict
	errorPage   string                  // Config.ErrorPage
	reactForced bool                    // Config.React, applied to a hot-file entry too

	manMu    sync.Mutex
	man      manifest // build manifest, cached once found
	manErr   error    // why the last manifest load failed
	manTried bool

	missingOnce sync.Once // the production "no assets" log line
	noMountOnce sync.Once // the production "no mount element" log line
	// headLoadsClient: Config.Head has a <script type="module">, so no
	// manifest is not a missing-assets problem.
	headLoadsClient bool
	logf            func(string, ...any) // defaults to log.Printf
}

// engineParams collects the registered SharedProviders from the fx value group
// populated by Share. The group defaults to empty when none are registered.
type engineParams struct {
	di.In
	Shared []SharedProvider `group:"inertia.shared"`
}

// Module wires the Inertia engine into an app: it provides the *Engine
// (constructed from Config plus any Share providers) and installs a global gin
// middleware that exposes the engine to page renderers and enforces the asset
// version check.
func Module(cfg Config) nexus.Option {
	if cfg.RootView == "" {
		cfg.RootView = "app"
	}
	return extension.Use(extension.Plugin{
		Name:    "inertia",
		Version: "1",
		Icon:    Icon,
		Options: []nexus.Option{
			nexus.Raw(di.Provide(func(app *nexus.App, in engineParams) *Engine {
				return newEngine(cfg, in.Shared, app)
			})),
			// Stash the engine on the app at boot. The page renderer pulls it
			// back via the appctx request key → App.Value at request time —
			// independent of gin-middleware install ordering, which di.Module route
			// registration can (and does) run ahead of. A plain engine.Use()
			// here would miss any inertia.Page declared inside a nexus.Module.
			nexus.Invoke(func(app *nexus.App, eng *Engine) {
				app.SetValue(engineKeyT{}, eng)
			}),
		},
	})
}

// newEngine builds the engine from Config + Share providers + the app (used to
// auto-discover ServeFrontend's bundle). Asset tags + version are resolved
// at render time (see assets), not here, so option order doesn't matter.
func newEngine(cfg Config, shared []SharedProvider, app *nexus.App) *Engine {
	entry := cfg.Entry
	if entry == "" {
		entry = "src/main.ts"
	}
	head := cfg.Head.render()
	return &Engine{
		rootView:        cfg.RootView,
		customHead:      head,
		headLoadsClient: loadsModule(head),
		shared:          shared,
		encryptHistory:  cfg.EncryptHistory,
		app:             app,
		cfgFrontend:     cfg.Frontend,
		cfgRoot:         cfg.Root,
		versionPin:      cfg.Version,
		devEntry:        entry,
		nonceFn:         cfg.Nonce,
		ssr:             cfg.SSR,
		onSSRError:      cfg.OnSSRError,
		ssrStrict:       cfg.SSRStrict,
		errorPage:       cfg.ErrorPage,
		reactForced:     cfg.React,
		logf:            log.Printf,
	}
}

// engineFromGin retrieves the per-app engine a page renderer needs, pulling it
// from the app stashed on the request context by the framework.
func engineFromGin(c *httpx.Ctx) (*Engine, bool) {
	a, _ := c.Get(appctx.Key)
	app, ok := a.(*nexus.App)
	if !ok || app == nil {
		return nil, false
	}
	v, ok := app.Value(engineKeyT{})
	if !ok {
		return nil, false
	}
	e, ok := v.(*Engine)
	return e, ok
}
