// Package dashboard mounts the nexus introspection surface under /__nexus.
// Ships a Vue dashboard (embedded from ui/dist), a JSON registry listing, and
// a WebSocket event stream.
package dashboard

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/extension/cron"
	"github.com/paulmanoni/nexus/live"
	"github.com/paulmanoni/nexus/manifest"
	"github.com/paulmanoni/nexus/extension/metrics"
	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

const Prefix = "/__nexus"

// Config carries the dashboard's runtime knobs: the brand the client
// fetches at startup + any gin middleware that should guard the
// /__nexus surface (auth, permission, allowlist, etc.).
type Config struct {
	Name       string            `json:"Name"`
	Middleware []gin.HandlerFunc `json:"-"`
	// Deployment is the unit name this binary boots as ("" = monolith).
	// Surfaced over /__nexus/config so the dashboard can render the
	// active deployment, and so peer services in a split deployment
	// can introspect it via federation.
	Deployment string `json:"Deployment,omitempty"`
	// Version is the binary's release tag (defaults to "dev"). Used by
	// generated cross-service clients to detect version skew.
	Version string `json:"Version,omitempty"`

	// Manifest is the closure dashboard.Mount calls on each request to
	// GET /__nexus/manifest. Returning the same value print mode
	// emits — typically `manifest.Build(app.manifestInputs())` — keeps
	// runtime and build-time JSON byte-equivalent (modulo
	// App.GeneratedAt, which is excluded from ManifestHash). When nil,
	// the manifest endpoint is not mounted.
	Manifest func() manifest.Manifest `json:"-"`

	// AdminToken gates GET /__nexus/manifest with a Bearer-token check.
	// Required for the endpoint to mount: empty token + non-nil
	// Manifest closure → endpoint stays unmounted (fail-closed). The
	// orchestration platform sets this via NEXUS_ADMIN_TOKEN at deploy
	// time; for local dev set it explicitly when you want to read the
	// runtime manifest.
	//
	// Compared in constant time against the request's
	// `Authorization: Bearer <token>` header.
	AdminToken string `json:"-"`

	// Plugins is the closure dashboard.Mount calls on each request to
	// GET /__nexus/plugins. Returns the inert metadata for every plugin
	// registered with the App — auth, oauth2, cron, ratelimit, etc.
	// nexus.New populates this by adapting app.Plugins() at mount time;
	// nil leaves the endpoint unmounted.
	Plugins func() []PluginInfo `json:"-"`
}

// PluginInfo mirrors nexus.PluginRecord in a form the dashboard can
// own without importing nexus (which already imports this package).
// nexus.New converts app.Plugins() into []PluginInfo at Mount time.
type PluginInfo struct {
	Name         string   `json:"name"`
	Version      string   `json:"version,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	HasDashboard bool     `json:"hasDashboard,omitempty"`
	HasClient    bool     `json:"hasClient,omitempty"`
	Tab          *TabInfo `json:"tab,omitempty"`
	LiveEvents   []string `json:"liveEvents,omitempty"`
}

// TabInfo is a plugin's declared dashboard nav-tab metadata.
type TabInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon,omitempty"`
}

// Mount attaches:
//
//	GET  /__nexus/config           -> Config JSON
//	GET  /__nexus/endpoints        -> services + endpoints from the registry
//	GET  /__nexus/resources        -> resource snapshots (health probed live)
//	GET  /__nexus/middlewares      -> middleware metadata
//	GET  /__nexus/crons            -> cron job snapshots (schedule, next/last run, history)
//	POST /__nexus/crons/:name/trigger -> run a job immediately (manual tick)
//	POST /__nexus/crons/:name/pause   -> pause scheduled ticks (manual Trigger still works)
//	POST /__nexus/crons/:name/resume  -> resume scheduled ticks
//	GET  /__nexus/events           -> WebSocket: backlog (since=N) then live trace events
//	GET  /__nexus/manifest         -> live manifest JSON (admin-token gated)
//	GET  /__nexus/, /assets/*      -> embedded Vue dashboard
//
// The events endpoint is only mounted if bus != nil. The manifest
// endpoint is only mounted when BOTH cfg.Manifest != nil AND
// cfg.AdminToken != "" — fail-closed so a forgotten env var doesn't
// silently expose service/env/cron declarations to the public. The
// cron + rate-limit + metrics endpoints are always mounted — their
// stores just return empty lists when nothing has been registered.
func Mount(e *gin.Engine, reg *registry.Registry, bus *trace.Bus, sched *cron.Scheduler, rl ratelimit.Store, ms metrics.Store, notifier *live.Notifier, cfg Config) {
	if cfg.Name == "" {
		cfg.Name = "Nexus"
	}
	g := e.Group(Prefix)
	// User-supplied gate (typically auth + permission). Applied to the
	// entire /__nexus group BEFORE any route registers, so it covers
	// the JSON API, the WebSocket event stream, and the embedded UI in
	// one stroke. Registration order is preserved — the first
	// middleware that aborts stops the chain.
	for _, mw := range cfg.Middleware {
		if mw != nil {
			g.Use(mw)
		}
	}
	// /config is dashboard-owned metadata — stays inline.
	g.GET("/config", func(c *gin.Context) {
		c.JSON(http.StatusOK, cfg)
	})
	// /plugins enumerates everything registered via app.RegisterPlugin
	// — built-ins (auth, oauth2, cron, ratelimit, metrics, cache,
	// dashboard) and any third-party extension.Use calls.
	if cfg.Plugins != nil {
		g.GET("/plugins", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"plugins": cfg.Plugins()})
		})
	}
	// Every other introspection surface lives in the package that owns
	// the data — keeps dashboard.Mount a thin orchestrator instead of
	// a god-route table.
	registry.MountDashboard(g, reg)
	if sched != nil {
		cron.MountDashboard(g, sched)
	}
	if rl != nil {
		ratelimit.MountDashboard(g, rl)
	}
	if ms != nil {
		metrics.MountDashboard(g, ms)
	}
	if bus != nil {
		trace.MountDashboard(g, bus)
	}
	// Manifest endpoint mounts only when both pieces are configured —
	// fail-closed so a missing NEXUS_ADMIN_TOKEN doesn't silently
	// expose service/env/cron declarations. The auth gate is route-
	// scoped (not group-scoped) because the rest of /__nexus is
	// gated by the operator-supplied cfg.Middleware; promoting the
	// admin-token gate to the whole group would change behavior for
	// every existing endpoint.
	if cfg.Manifest != nil && cfg.AdminToken != "" {
		g.GET("/manifest", adminTokenGate(cfg.AdminToken), func(c *gin.Context) {
			c.JSON(http.StatusOK, cfg.Manifest())
		})
	}
	// /live is the consolidated WS pump that replaces the dashboard's
	// 5-second poll. One socket carries periodic snapshots of every
	// surface (endpoints, resources, workers, stats, crons, ratelimits)
	// — the UI subscribes once and renders live. /events stays separate
	// for per-request trace pulses.
	g.GET("/live", streamLive(reg, ms, sched, rl, notifier))
	mountUI(g)
}

// adminTokenGate returns a middleware that 401s any request whose
// `Authorization: Bearer <token>` header doesn't match expected.
// Comparison is constant-time so an attacker can't measure response
// latency to recover the token byte-by-byte.
//
// Why a hand-rolled gate instead of leaning on cfg.Middleware: the
// orchestration platform needs a *predictable* contract — "set
// NEXUS_ADMIN_TOKEN, get a Bearer-gated endpoint" — independent of
// whatever auth middleware the app's operator wired for the rest of
// /__nexus. A built-in gate makes the orchestrator's assumption
// stable across apps.
func adminTokenGate(expected string) gin.HandlerFunc {
	expectedBytes := []byte(expected)
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(header, prefix) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
			return
		}
		got := []byte(header[len(prefix):])
		// subtle.ConstantTimeCompare returns 1 only when the byte
		// slices have equal length AND equal contents. Differing
		// lengths fail without short-circuiting, which is exactly
		// what we want to avoid leaking token length.
		if subtle.ConstantTimeCompare(got, expectedBytes) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid admin token"})
			return
		}
		c.Next()
	}
}
