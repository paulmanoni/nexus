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
	"os"
	"strings"
	"sync"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// AutoVersion is the zero value for Config.Version: derive the Inertia asset
// version from a hash of the build manifest. Set Config.Version to a fixed
// string to pin it instead.
const AutoVersion = ""

// devURLEnv is set by `nexus dev` to the viteless/Vite dev server URL so the
// shell can reference the HMR client when no build manifest is present.
const devURLEnv = "NEXUS_VITE_DEV"

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
	// Defaults to "app".
	RootView string
	// Version is the Inertia asset version. Empty (AutoVersion) derives it
	// from the manifest hash; a fixed string pins it.
	Version string
	// Head is the document <head> for the shell — the app's title, meta, and
	// stylesheet/font links that would otherwise live in index.html. The engine
	// renders its own minimal shell, so anything index.html declared in <head>
	// must be supplied here to reach a full-page load. charset/viewport and the
	// Vite/manifest asset tags are added automatically. See the Head type for a
	// Raw escape hatch.
	Head Head
	// EncryptHistory turns on Inertia history-state encryption for every page
	// by default (Inertia v2). A handler can override per-response with
	// inertia.EncryptHistory(c, false); inertia.ClearHistory(c) drops any
	// previously-encrypted entry (e.g. on logout).
	EncryptHistory bool
	// Entry is the dev-server module the shell loads under `nexus dev` (the
	// client app's entry, served by the Vite/viteless dev server). Defaults to
	// "src/main.ts"; set "src/main.tsx" for a React app. Ignored in production,
	// where the entry comes from the build manifest.
	Entry string
	// React emits the Vite React Fast Refresh preamble before the dev client so
	// HMR works for React apps. Auto-enabled when Entry ends in .tsx/.jsx; set
	// it explicitly to force the preamble for a .ts/.js React entry.
	React bool
	// Nonce returns the per-request CSP nonce for the document shell. When set
	// and non-empty, the engine stamps nonce="…" on every <script>/<link> it
	// injects (asset tags + dev preamble + Config.Head), so they satisfy a
	// strict `script-src 'nonce-…'` / `style-src 'nonce-…'` policy. The app's
	// CSP middleware owns generating the nonce + setting the Content-Security-
	// Policy header; return that same value here. Leave nil for no CSP nonce.
	Nonce func(*httpx.Ctx) string
}

// Engine renders Inertia responses for an app. One is built per app via Module
// and shared across requests. The asset head/version are resolved lazily on the
// first render (see resolve) so the bundle ServeFrontend registers is visible
// regardless of option ordering.
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
	react       bool                    // emit the React Fast Refresh preamble in dev
	nonceFn     func(*httpx.Ctx) string // per-request CSP nonce (Config.Nonce)

	resolveOnce sync.Once
	head        string // resolved <head> asset tags (prod manifest or dev server)
	version     string // resolved Inertia asset version
}

// resolve computes the asset head tags + version once, on first use. Deferring
// past boot lets ServeFrontend's bundle registration land first no matter the
// option order. Dev (NEXUS_VITE_DEV) wins; otherwise read the Vite manifest
// from Config.Frontend or, failing that, the app's ServeFrontend bundle.
func (e *Engine) resolve() {
	e.resolveOnce.Do(func() {
		if dev := strings.TrimRight(os.Getenv(devURLEnv), "/"); dev != "" {
			e.head = devHeadTags(dev, e.devEntry, e.react)
			return
		}
		fsys, root := e.cfgFrontend, e.cfgRoot
		if fsys == nil && e.app != nil {
			if f, r, ok := e.app.FrontendFS(); ok {
				fsys, root = f, r
			}
		}
		var man manifest
		if fsys != nil {
			if m, err := loadManifest(fsys, root); err == nil {
				man = m
			}
		}
		if man.found {
			e.head = man.headTags()
		}
		if e.versionPin != AutoVersion {
			e.version = e.versionPin
		} else {
			e.version = man.version
		}
	})
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
			// back via AppFromGin(c) → App.Value at request time — independent
			// of gin-middleware install ordering, which di.Module route
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
// lazily on first render (see resolve), not here, so option order doesn't
// matter.
func newEngine(cfg Config, shared []SharedProvider, app *nexus.App) *Engine {
	entry := cfg.Entry
	if entry == "" {
		entry = "src/main.ts"
	}
	return &Engine{
		rootView:       cfg.RootView,
		customHead:     cfg.Head.render(),
		shared:         shared,
		encryptHistory: cfg.EncryptHistory,
		app:            app,
		cfgFrontend:    cfg.Frontend,
		cfgRoot:        cfg.Root,
		versionPin:     cfg.Version,
		devEntry:       entry,
		// Auto-detect React from a JSX entry; Config.React forces it on.
		react:   cfg.React || strings.HasSuffix(entry, ".tsx") || strings.HasSuffix(entry, ".jsx"),
		nonceFn: cfg.Nonce,
	}
}

// engineFromGin retrieves the per-app engine a page renderer needs, pulling it
// from the app stashed on the request context by the framework.
func engineFromGin(c *httpx.Ctx) (*Engine, bool) {
	app, ok := nexus.AppFromGin(c)
	if !ok {
		return nil, false
	}
	v, ok := app.Value(engineKeyT{})
	if !ok {
		return nil, false
	}
	e, ok := v.(*Engine)
	return e, ok
}
