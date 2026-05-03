// Package dashboard mounts the nexus introspection surface under /__nexus.
// Ships a Vue dashboard (embedded from ui/dist), a JSON registry listing, and
// a WebSocket event stream.
package dashboard

import (
	"context"
	"crypto/subtle"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/cron"
	"github.com/paulmanoni/nexus/manifest"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/ratelimit"
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
func Mount(e *gin.Engine, reg *registry.Registry, bus *trace.Bus, sched *cron.Scheduler, rl ratelimit.Store, ms metrics.Store, cfg Config) {
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
	g.GET("/config", func(c *gin.Context) {
		c.JSON(http.StatusOK, cfg)
	})
	g.GET("/endpoints", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"services":  reg.Services(),
			"endpoints": reg.Endpoints(),
		})
	})
	g.GET("/resources", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"resources": reg.Resources()})
	})
	g.GET("/workers", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workers": reg.Workers()})
	})
	g.GET("/middlewares", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"middlewares": reg.Middlewares(),
			"global":      reg.GlobalMiddlewares(),
		})
	})
	if sched != nil {
		mountCron(g, sched)
	}
	if rl != nil {
		mountRateLimits(g, rl)
	}
	if ms != nil {
		g.GET("/stats", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"stats": ms.Snapshot()})
		})
		// Per-op error ring — lazy-loaded when the dashboard opens the
		// error dialog for a specific endpoint. Keeps /stats lean even
		// when RecentErrorsCap is in the thousands.
		//
		// service + op live in query parameters (not :path/:params)
		// because REST op names are "<METHOD> <path>" and contain
		// slashes — which gin's path-param matcher can't capture
		// across segment boundaries. Query params handle the URL-
		// encoding cleanly.
		g.GET("/stats/errors", func(c *gin.Context) {
			s := c.Query("service")
			o := c.Query("op")
			key := s + "." + o
			c.JSON(http.StatusOK, gin.H{
				"key":    key,
				"events": ms.Errors(key),
			})
		})
	}
	if bus != nil {
		g.GET("/events", streamEvents(bus))
		g.GET("/traces/:id", traceByID(bus))
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
	g.GET("/live", streamLive(reg, ms, sched, rl))
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

// traceSpan is one node in the waterfall. Times are relative to the trace's
// earliest event so the UI can render bars without knowing absolute clock.
type traceSpan struct {
	SpanID     string         `json:"spanId"`
	ParentID   string         `json:"parentId,omitempty"`
	Name       string         `json:"name"`
	Kind       string         `json:"kind"`
	Service    string         `json:"service,omitempty"`
	Endpoint   string         `json:"endpoint,omitempty"`
	Transport  string         `json:"transport,omitempty"`
	StartMs    int64          `json:"startMs"`
	DurationMs int64          `json:"durationMs"`
	Status     int            `json:"status,omitempty"`
	Error      string         `json:"error,omitempty"`
	Remote     bool           `json:"remote,omitempty"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

// traceByID reconstructs a span tree from the ring buffer. Merges
// request.start / request.end (the root) and span.start / span.end (children)
// into one node per SpanID. Events without a SpanID (e.g. KindDownstream
// markers) are skipped — they'd have no bar to render.
func traceByID(bus *trace.Bus) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.Param("id")
		events := bus.SnapshotByTrace(id)
		if len(events) == 0 {
			c.JSON(http.StatusNotFound, gin.H{"error": "trace not found"})
			return
		}
		base := events[0].Timestamp
		for _, e := range events {
			if !e.Timestamp.IsZero() && e.Timestamp.Before(base) {
				base = e.Timestamp
			}
		}
		spans := map[string]*traceSpan{}
		for _, e := range events {
			if e.SpanID == "" {
				continue
			}
			node, ok := spans[e.SpanID]
			if !ok {
				node = &traceSpan{
					SpanID:   e.SpanID,
					ParentID: e.ParentID,
					Service:  e.Service,
					Endpoint: e.Endpoint,
					Remote:   e.Remote,
				}
				spans[e.SpanID] = node
			}
			if e.Name != "" {
				node.Name = e.Name
			}
			if e.Transport != "" {
				node.Transport = e.Transport
			}
			switch e.Kind {
			case trace.KindRequestStart, trace.KindSpanStart:
				node.Kind = string(e.Kind)
				if !e.Timestamp.IsZero() {
					node.StartMs = e.Timestamp.Sub(base).Milliseconds()
				}
				if e.Meta != nil {
					node.Attrs = e.Meta
				}
			case trace.KindRequestEnd, trace.KindSpanEnd:
				node.DurationMs = e.DurationMs
				if e.Error != "" {
					node.Error = e.Error
				}
				if e.Status != 0 {
					node.Status = e.Status
				}
				if e.Meta != nil {
					node.Attrs = e.Meta
				}
			}
			if node.Name == "" {
				node.Name = e.Endpoint
			}
		}
		out := make([]*traceSpan, 0, len(spans))
		for _, s := range spans {
			out = append(out, s)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].StartMs != out[j].StartMs {
				return out[i].StartMs < out[j].StartMs
			}
			return out[i].SpanID < out[j].SpanID
		})
		c.JSON(http.StatusOK, gin.H{"traceId": id, "spans": out})
	}
}

// mountRateLimits serves the rate-limit introspection + override surface:
//
//	GET    /__nexus/ratelimits                    → snapshot of every key
//	POST   /__nexus/ratelimits?service=...&op=... → override limit live
//	DELETE /__nexus/ratelimits?service=...&op=... → reset to declared baseline
//
// service + op live in QUERY params (not :path/:params) because REST
// op names are "<METHOD> <path>" and contain slashes — which gin's
// path-param matcher can't capture across segment boundaries. Query
// params handle the URL-encoding cleanly.
//
// The key format is "<service>.<op>" — matches what the auto-mount
// registers at boot so dashboard and store talk the same dialect.
func mountRateLimits(g *gin.RouterGroup, store ratelimit.Store) {
	g.GET("/ratelimits", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"limits": store.Snapshot(c.Request.Context())})
	})
	g.POST("/ratelimits", func(c *gin.Context) {
		var body ratelimit.Limit
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		key := c.Query("service") + "." + c.Query("op")
		rec, err := store.Configure(c.Request.Context(), key, body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, rec)
	})
	g.DELETE("/ratelimits", func(c *gin.Context) {
		key := c.Query("service") + "." + c.Query("op")
		if err := store.Reset(c.Request.Context(), key); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
}

// ctxBg is a package-level convenience import binder — keeps the
// `context` import needed by mountRateLimits live even if a future
// refactor moves Allow calls out to middleware files.
var _ = context.Background

func mountCron(g *gin.RouterGroup, sched *cron.Scheduler) {
	g.GET("/crons", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"crons": sched.Snapshots()})
	})
	g.POST("/crons/:name/trigger", func(c *gin.Context) {
		if !sched.Trigger(c.Param("name")) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"ok": true})
	})
	g.POST("/crons/:name/pause", func(c *gin.Context) {
		if !sched.SetPaused(c.Param("name"), true) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"paused": true})
	})
	g.POST("/crons/:name/resume", func(c *gin.Context) {
		if !sched.SetPaused(c.Param("name"), false) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"paused": false})
	})
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func streamEvents(bus *trace.Bus) gin.HandlerFunc {
	return func(c *gin.Context) {
		var since int64
		if s := c.Query("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		backlog, ch, cancel := bus.Subscribe(since, 128)
		defer cancel()

		for _, e := range backlog {
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		}
		for e := range ch {
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		}
	}
}
