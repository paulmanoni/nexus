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
//	    nexus.ServeFrontend(webFS, "web/dist"),          // hashed JS/CSS assets
//	    inertia.Module(inertia.Config{                   // the page protocol
//	        Frontend: webFS, Root: "web/dist",
//	    }),
//	    inertia.Share(SharedAuth),
//	    inertia.Page("GET", "/users", "Users/Index", NewListUsers),
//	)
package inertia

import (
	"io/fs"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

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
	// Frontend is the embedded (or dev disk) filesystem holding the built
	// bundle — the same FS passed to nexus.ServeFrontend. Used to read the
	// Vite manifest for production asset tags and the auto version.
	Frontend fs.FS
	// Root is the path within Frontend to the build output (e.g.
	// "web/dist"). The manifest is read from Root/.vite/manifest.json.
	Root string
	// RootView is the id of the root element the Inertia client mounts on.
	// Defaults to "app".
	RootView string
	// Version is the Inertia asset version. Empty (AutoVersion) derives it
	// from the manifest hash; a fixed string pins it.
	Version string
	// Head is raw HTML injected into the document shell's <head> — the place
	// for the app's title, meta, favicon, fonts, and any global stylesheets
	// (e.g. an icon font or a component-library CSS) that would otherwise live
	// in index.html. The engine renders its own minimal shell, so anything
	// index.html declared in <head> must be supplied here to reach a full-page
	// load. The framework's charset/viewport meta and the Vite/manifest asset
	// tags are added automatically.
	Head string
}

// Engine renders Inertia responses for an app. One is built per app via Module
// and shared (read-only) across requests.
type Engine struct {
	rootView   string
	version    string
	head       string // resolved <head> asset tags (prod manifest or dev server)
	customHead string // app-supplied <head> HTML (Config.Head)
	shared     []SharedProvider
}

// engineParams collects the registered SharedProviders from the fx value group
// populated by Share. The group defaults to empty when none are registered.
type engineParams struct {
	fx.In
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
		Options: []nexus.Option{
			nexus.Raw(fx.Provide(func(in engineParams) *Engine {
				return newEngine(cfg, in.Shared)
			})),
			// Stash the engine on the app at boot. The page renderer pulls it
			// back via AppFromGin(c) → App.Value at request time — independent
			// of gin-middleware install ordering, which fx.Module route
			// registration can (and does) run ahead of. A plain engine.Use()
			// here would miss any inertia.Page declared inside a nexus.Module.
			nexus.Invoke(func(app *nexus.App, eng *Engine) {
				app.SetValue(engineKeyT{}, eng)
			}),
		},
	})
}

// newEngine resolves the asset tags and version from the build manifest, with
// graceful fallbacks: a dev server URL (NEXUS_VITE_DEV) when no manifest is
// present, and an empty version when neither is available.
func newEngine(cfg Config, shared []SharedProvider) *Engine {
	e := &Engine{rootView: cfg.RootView, customHead: cfg.Head, shared: shared}

	// Dev takes precedence: when a Vite/viteless dev server is running
	// (`nexus dev` sets NEXUS_VITE_DEV), reference it for HMR and IGNORE any
	// build manifest. The embedded manifest is frozen at go-build time and goes
	// stale the moment the frontend rebuilds — pointing the shell at hashed
	// /assets/* that no longer exist. No asset version in dev; the dev server
	// owns reloads, so the version guard stays off (e.version == "").
	if dev := strings.TrimRight(os.Getenv(devURLEnv), "/"); dev != "" {
		e.head = devHeadTags(dev)
		return e
	}

	// Production: resolve hashed assets + version from the build manifest.
	var man manifest
	if cfg.Frontend != nil {
		if m, err := loadManifest(cfg.Frontend, cfg.Root); err == nil {
			man = m
		}
	}
	if man.found {
		e.head = man.headTags()
	}
	if cfg.Version != AutoVersion {
		e.version = cfg.Version
	} else {
		e.version = man.version
	}
	return e
}

// engineFromGin retrieves the per-app engine a page renderer needs, pulling it
// from the app stashed on the request context by the framework.
func engineFromGin(c *gin.Context) (*Engine, bool) {
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
