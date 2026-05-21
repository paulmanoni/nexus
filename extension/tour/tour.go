// Package tour ships a Spring-Boot-style guided-tour plugin for
// nexus apps. The plugin captures click-by-click walkthroughs of
// any HTTP-served frontend (React, Vue, Angular, vanilla — all
// supported), then plays them back as numbered-badge highlights
// over the same UI. The in-page agent renders inside a Shadow DOM
// rooted at document.body so host CSS can never bleed in and the
// plugin's UI can never bleed out.
//
// Phase 1 (this file) ships the Go side: storage (in-memory or
// gorm-backed), REST handlers under /__nexus/tour/..., the
// /inject.js bootstrap, and an optional middleware that
// auto-injects the script tag into every HTML response. Phase 2
// will replace the Phase-1 inject.js stub with the full TS-built
// recorder/picker/runner bundle; Phase 3 polishes the dashboard
// UI for authoring.
//
// Typical wiring:
//
//	nexus.Run(nexus.Config{...},
//	    tour.Module(
//	        tour.WithGORM(db.DB()),  // production persistence
//	        tour.AutoInject(true),   // splice <script> into every HTML response
//	    ),
//	    // ... rest of the app
//	)
package tour

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
	"gorm.io/gorm"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Option is the functional-option shape for Module.
type Option interface{ apply(*config) }

type config struct {
	store       Store
	storeErr    error // deferred from WithGORM so Module can fail boot cleanly
	autoInject  bool
	dashboardOn bool
}

func defaultConfig() config {
	return config{
		dashboardOn: true, // tab is on by default; operators opt out via NoDashboard()
	}
}

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

// WithStore wires a custom Store. Most operators use WithGORM
// instead; WithStore is the escape hatch for a custom backend
// (Redis, file-system, S3 manifest, etc.).
func WithStore(s Store) Option {
	return optionFunc(func(c *config) { c.store = s })
}

// WithGORM is the production wiring — points the plugin at the
// app's *gorm.DB. AutoMigrate runs immediately so a missing-
// permission / locked-DB situation surfaces at option construction
// instead of on the first request.
func WithGORM(db *gorm.DB) Option {
	return optionFunc(func(c *config) {
		if db == nil {
			return
		}
		gs, err := NewGormStore(db)
		if err != nil {
			c.storeErr = err
			return
		}
		c.store = gs
	})
}

// AutoInject toggles the HTML-response middleware that splices a
// <script src="/__nexus/tour/inject.js"> tag into every HTML page
// the host app serves. Default false — opt in once you're sure
// the in-page overlay won't conflict with host-app workflows.
func AutoInject(on bool) Option {
	return optionFunc(func(c *config) { c.autoInject = on })
}

// NoDashboard disables the dashboard nav tab. The REST routes
// stay mounted (the in-page agent still needs them); only the
// "Tours" tab on /__nexus/ goes away.
func NoDashboard() Option {
	return optionFunc(func(c *config) { c.dashboardOn = false })
}

// Module is the plugin entry point. Returns a nexus.Option you
// pass to nexus.Run alongside the rest of your app's modules.
func Module(opts ...Option) nexus.Option {
	cfg := defaultConfig()
	for _, o := range opts {
		o.apply(&cfg)
	}
	// Surface any WithGORM AutoMigrate failure as an fx.Error so
	// boot stops cleanly rather than the plugin silently falling
	// back to the in-memory store.
	if cfg.storeErr != nil {
		return nexus.Raw(fx.Error(cfg.storeErr))
	}
	// Default store is in-memory — fine for dev, demos, and any
	// app that doesn't need cross-restart persistence.
	if cfg.store == nil {
		cfg.store = NewMemoryStore()
	}

	h := &handlers{store: cfg.store}

	plugin := extension.Plugin{
		Name:    "tour",
		Version: "0.1.0",
		Options: []nexus.Option{
			// Mount the in-page agent + the activeForRoute lookup
			// outside /__nexus/ so the host frontend can fetch
			// them on the same origin without auth gates that
			// might wrap /__nexus/.
			nexus.Invoke(func(app *nexus.App) {
				eng := app.Engine()
				eng.GET("/__nexus/tour/inject.js", handleInjectJS)
				eng.GET("/__nexus/tour/active", h.activeForRoute)
				if cfg.autoInject {
					eng.Use(autoInjectMiddleware())
				}
			}),
		},
		Dashboard: &extension.Dashboard{
			Routes: []extension.Route{
				// Path "" lands at /__nexus/tour — the
				// management UI. Operators land here from the
				// dashboard tab; the in-page agent script tag
				// gets spliced in too (if AutoInject is on),
				// so "edit + try it" is one page reload.
				{Method: "GET", Path: "", Handler: handleDashboard},
				{Method: "GET", Path: "/tours", Handler: h.listTours},
				{Method: "GET", Path: "/tours/:id", Handler: h.getTour},
				{Method: "GET", Path: "/preview", Handler: handlePreview},
				{Method: "GET", Path: "/tours/:id/preview", Handler: handlePreview},
				{Method: "POST", Path: "/tours", Handler: h.upsertTour},
				{Method: "DELETE", Path: "/tours/:id", Handler: h.deleteTour},
				{Method: "POST", Path: "/tours/reorder", Handler: h.reorderTours},
				{Method: "POST", Path: "/tours/:id/reorder", Handler: h.reorderSteps},
				{Method: "POST", Path: "/steps", Handler: h.upsertStep},
				{Method: "DELETE", Path: "/steps/:id", Handler: h.deleteStep},
			},
		},
	}

	if cfg.dashboardOn {
		plugin.Dashboard.Tab = &extension.Tab{
			ID:    "tour",
			Label: "Tours",
			Icon:  "play-circle",
		}
	}

	return extension.Use(plugin)
}

// _ guards: ensures every handler is referenced from this file so
// future refactors don't quietly drop one from the route table.
var _ = []gin.HandlerFunc{
	handleInjectJS,
}
