// Package visitors counts page views on a nexus app and exposes the
// totals over a small public API the frontend polls. Designed for
// marketing sites and consumer-facing apps where "X visitors / Y
// online right now" doubles as social proof on the page.
//
// The plugin runs SPA-driven: it does not silently middleware-tap
// every HTTP request (which would conflate API calls with page
// views). Instead the frontend explicitly POSTs to /api/visitors/track
// on each route change. That keeps the counts honest — one mount =
// one view, even when a single user hits twelve /api/* endpoints
// to render the page.
//
//	import "github.com/paulmanoni/nexus/extension/visitors"
//
//	nexus.Run(
//	    nexus.Config{...},
//	    visitors.Plugin(visitors.Config{
//	        StorePath: "data/visitors.json",
//	    }),
//	    // ... rest of the app
//	)
//
// What the plugin exposes:
//
//	POST /api/visitors/track       SPA pings on each route change
//	GET  /api/visitors/stats       returns counts as JSON
//	GET  /__nexus/visitors/stats   admin variant (same data)
//	GET  /__nexus/visitors/top     top paths by view count
//	POST /__nexus/visitors/reset   wipe all counters (admin)
//
// Identification: a long-lived first-party cookie (`nx_visitor`) is
// set on first track. Cookieless visitors fall back to "all share
// one bucket" — we don't fingerprint, on purpose. Operators who
// want stricter tracking can attach the auth identity in a custom
// Identify function.
package visitors

import (
	"context"
	"fmt"
	"time"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Config controls the plugin's policy. Sensible defaults; only
// StorePath is worth setting in production (so counts survive
// restarts).
type Config struct {
	// StorePath is the on-disk JSON file the counter persists to.
	// Defaults to "data/visitors.json". Override via the env var
	// NEXUS_VISITORS_STORE — useful when WorkingDirectory + the
	// systemd ProtectHome/Strict modes lock out the default path.
	StorePath string

	// APIPath is the prefix the public track + stats endpoints
	// mount under. Defaults to "/api/visitors".
	APIPath string

	// CookieName is the visitor-ID cookie. Defaults to
	// "nx_visitor". 32-hex random value; set on first track,
	// trusted on subsequent tracks for unique-visitor accounting.
	CookieName string

	// CookieMaxAgeDays controls the cookie's persistence. Default
	// 365. Set to 1 for a "session counter" feel; the unique-
	// visitor count then resets daily.
	CookieMaxAgeDays int

	// OnlineWindow is the time window an active visitor counts as
	// "online now". Default 60 seconds. Set higher for slow-poll
	// frontends, lower for high-traffic feel.
	OnlineWindow time.Duration

	// SaveEvery is the interval the counter flushes to disk. Per-
	// request saves would be too expensive for high-traffic apps.
	// 30s strikes a balance — a crash loses at most 30s of
	// counter delta, the on-disk JSON stays fresh-ish for ops.
	SaveEvery time.Duration

	// TopPaths is the max number of distinct paths the plugin
	// tracks for the "top pages" view. Bounded so a malicious
	// client can't blow memory by pinging /track with random
	// paths. Default 100; LRU-style eviction past that.
	TopPaths int

	// Disabled, when true, makes the plugin a no-op. Useful for
	// preview environments where counter noise doesn't matter.
	Disabled bool
}

// Plugin wires the visitor counter into the app's lifecycle.
func Plugin(cfg Config) nexus.Option {
	applyDefaults(&cfg)
	state := &pluginState{cfg: cfg}

	return extension.Use(extension.Plugin{
		Name:    "visitors",
		Version: "0.1.0",

		// Options registers the public API routes via direct
		// engine.GET/POST so they land at /api/visitors/* rather
		// than under /__nexus/. Runs during fx.New (before
		// AsRest invokes execute), so the routes are in place
		// before the framework starts serving.
		Options: []nexus.Option{
			nexus.Invoke(func(app *nexus.App) {
				state.app = app
				eng := app.Engine()
				api := cfg.APIPath
				eng.POST(api+"/track", state.handleTrack)
				eng.GET(api+"/stats", state.handlePublicStats)
			}),
		},

		Lifecycle: &extension.Lifecycle{
			// OnBoot loads the persisted counters off disk and
			// starts the periodic save goroutine. Doing this
			// before listeners bind means the first request
			// after restart sees the resurrected counters, not
			// a fresh zero.
			OnBoot:     state.boot,
			OnShutdown: state.stop,
		},

		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "visitors",
				Label: "Visitors",
				Icon:  "users",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/stats", Handler: state.handleAdminStats},
				{Method: "GET", Path: "/top", Handler: state.handleTopPaths},
				{Method: "POST", Path: "/reset", Handler: state.handleReset},
			},
		},
	})
}

// pluginState carries the resolved Config + the live Counter + the
// periodic-save cancel function. Created once in Plugin() so HTTP
// handlers can close over it.
type pluginState struct {
	cfg       Config
	app       *nexus.App
	counter   *Counter
	cancelSav func()
}

// boot loads persisted counters off disk + starts the save loop.
// Doesn't fail boot if the store file is missing or unreadable —
// that's the first-run case (no data yet) and the operator-error
// case (path typo); both are recoverable, neither should crash the
// app. Logs the error and proceeds with an empty counter.
func (s *pluginState) boot(ctx context.Context, app *nexus.App) error {
	if s.cfg.Disabled {
		return nil
	}
	s.counter = NewCounter(s.cfg.OnlineWindow, s.cfg.TopPaths)
	if err := s.counter.LoadFromFile(s.cfg.StorePath); err != nil {
		// Logged but not fatal — see comment above.
		fmt.Printf("nexus visitors: load %s: %v (starting fresh)\n", s.cfg.StorePath, err)
	}

	saveCtx, cancel := context.WithCancel(context.Background())
	s.cancelSav = cancel
	go s.savingLoop(saveCtx)
	return nil
}

// stop flushes one final time + cancels the save goroutine. Honors
// the OnShutdown context's deadline so a slow disk doesn't block
// the rest of fx's tear-down.
func (s *pluginState) stop(ctx context.Context) error {
	if s.cancelSav != nil {
		s.cancelSav()
	}
	if s.counter != nil {
		// Best-effort final flush — errors logged not returned,
		// because returning would block the shutdown of every
		// other plugin behind a disk write.
		if err := s.counter.SaveToFile(s.cfg.StorePath); err != nil {
			fmt.Printf("nexus visitors: final save: %v\n", err)
		}
	}
	return nil
}

// savingLoop is the long-lived goroutine that flushes the counter
// to disk every Config.SaveEvery. Exits when ctx is canceled (which
// happens in stop()).
func (s *pluginState) savingLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.SaveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.counter.SaveToFile(s.cfg.StorePath); err != nil {
				fmt.Printf("nexus visitors: periodic save: %v\n", err)
			}
		}
	}
}

func applyDefaults(cfg *Config) {
	if cfg.StorePath == "" {
		cfg.StorePath = "data/visitors.json"
	}
	if cfg.APIPath == "" {
		cfg.APIPath = "/api/visitors"
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "nx_visitor"
	}
	if cfg.CookieMaxAgeDays == 0 {
		cfg.CookieMaxAgeDays = 365
	}
	if cfg.OnlineWindow == 0 {
		cfg.OnlineWindow = 60 * time.Second
	}
	if cfg.SaveEvery == 0 {
		cfg.SaveEvery = 30 * time.Second
	}
	if cfg.TopPaths == 0 {
		cfg.TopPaths = 100
	}
}
