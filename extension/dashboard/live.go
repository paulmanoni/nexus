package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"github.com/paulmanoni/nexus/httpx"

	"github.com/paulmanoni/nexus/extension/cron"
	"github.com/paulmanoni/nexus/extension/metrics"
	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/transport/gql"
)

// heartbeatInterval is the maximum time between snapshot emissions
// when no changes have fired. Push-on-change is the primary path
// (registry mutations call live.Notifier.Notify which wakes the
// stream); the heartbeat exists as a safety net so a paused app
// still gives the dashboard a recent timestamp + lets the client
// confirm the connection is alive. 5s is generous: an idle app
// genuinely has nothing to say, and the client's auto-reconnect
// covers a missed heartbeat.
const heartbeatInterval = 5 * time.Second

// debounceWindow coalesces a burst of mutations into one snapshot
// send. fx graph construction registers ~50 endpoints in <1ms; we
// don't want to fire 50 snapshots through the WS — one snapshot
// after the storm settles is enough. 50ms is short enough that an
// interactive operator action (clicking pause on a cron) feels
// instant while still folding the cascade of internal updates that
// follow it.
const debounceWindow = 50 * time.Millisecond

// liveSnapshot bundles every source the dashboard renders so a single WS
// frame replaces the old (endpoints + resources + workers + stats + crons +
// ratelimits) poll fan-out. Optional subsystems (ms / sched / rl) emit nil
// fields — `omitempty` keeps the payload tight.
type liveSnapshot struct {
	Kind         string                      `json:"kind"` // always "snapshot"
	TS           time.Time                   `json:"ts"`
	Services     []registry.Service          `json:"services,omitempty"`
	Endpoints    []registry.Endpoint         `json:"endpoints,omitempty"`
	Resources    []registry.ResourceSnapshot `json:"resources,omitempty"`
	Workers      []registry.Worker           `json:"workers,omitempty"`
	Stats        []metrics.EndpointStats     `json:"stats,omitempty"`
	Crons        []cron.Snapshot             `json:"crons,omitempty"`
	RateLimits   []ratelimit.Record          `json:"ratelimits,omitempty"`
	GraphQLCache []gql.MountCacheStats       `json:"graphqlCache,omitempty"`
	// Middlewares + Global stream the registered middleware catalogue and
	// the ordered global chain so the dashboard renders them live instead
	// of polling GET /__nexus/middlewares.
	Middlewares []middleware.Info `json:"middlewares,omitempty"`
	Global      []string          `json:"global,omitempty"`
	// Extra carries optional plugin-contributed payloads (e.g. auth's
	// cached-identity summary) registered via RegisterSnapshotExtra, so
	// plugin live state streams over the WS rather than being polled.
	Extra map[string]any `json:"extra,omitempty"`
}

// streamLive is the WS handler at /__nexus/live. Push-driven: a
// registry mutation (RegisterEndpoint, UpdateWorkerStatus, …) calls
// notifier.Notify, the writer wakes within debounceWindow, builds a
// snapshot, and ships it. A heartbeat ticker fires the same path
// every heartbeatInterval as a fallback so an idle app still emits
// its current state periodically.
//
// Identical-snapshot dedup: SHA256 of the marshaled payload is
// kept across iterations; sends are skipped when the hash matches.
// Prevents wasted bytes when a notify fires for a mutation that
// produced an identical observable state (e.g. attaching a
// resource that's already attached).
//
// The writer never blocks indefinitely — write deadlines force
// errors on backpressure, NextReader signals client close, ctx
// covers server shutdown.
// Notifier is the minimum surface the dashboard needs from the
// framework's pub-sub primitive. Defined locally so this package
// stays a leaf — the concrete *nexus.Notifier (which holds the
// real implementation) satisfies this interface structurally and
// the root package can pass it in without us importing nexus and
// creating an import cycle.
type Notifier interface {
	Subscribe() (<-chan struct{}, func())
}

func streamLive(reg *registry.Registry, ms metrics.Store, sched *cron.Scheduler, rl ratelimit.Store, gqlStats *gql.StatsRegistry, notifier Notifier) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		ctx := c.Request.Context()

		// Subscribe BEFORE the initial snapshot so any notify firing
		// between snapshot construction and channel registration is
		// captured (will trigger a redundant immediate send, deduped
		// by the hash).
		var nudge <-chan struct{}
		var nudgeCancel func()
		if notifier != nil {
			nudge, nudgeCancel = notifier.Subscribe()
			defer nudgeCancel()
		}

		buildSnap := func() liveSnapshot {
			snap := liveSnapshot{
				Kind:        "snapshot",
				TS:          time.Now(),
				Services:    reg.Services(),
				Endpoints:   reg.VisibleEndpoints(),
				Resources:   reg.Resources(),
				Workers:     reg.Workers(),
				Middlewares: reg.Middlewares(),
				Global:      reg.GlobalMiddlewares(),
				Extra:       snapshotExtras(),
			}
			if ms != nil {
				snap.Stats = ms.Snapshot()
			}
			if sched != nil {
				snap.Crons = sched.Snapshots()
			}
			if rl != nil {
				snap.RateLimits = rl.Snapshot(ctx)
			}
			// gqlStats may be nil for apps that don't auto-mount
			// GraphQL; Snapshot() tolerates nil and returns no
			// rows so omitempty drops the field.
			snap.GraphQLCache = gqlStats.Snapshot()
			return snap
		}

		var lastHash [32]byte
		send := func(force bool) error {
			snap := buildSnap()
			body, err := json.Marshal(snap)
			if err != nil {
				return err
			}
			h := sha256.Sum256(body)
			if !force && h == lastHash {
				// State hasn't observably changed — skip the send.
				return nil
			}
			lastHash = h
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteMessage(1, body)
		}

		// Initial snapshot — force=true so the client sees state on
		// first connect even if the hash happens to match a future
		// no-op (impossible at boot, but the explicit force is the
		// right contract).
		if err := send(true); err != nil {
			return
		}

		// Detect client close so a half-open conn (browser tab
		// closed, network blip) stops blocking on the writer.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.NextReader(); err != nil {
					return
				}
			}
		}()

		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		// debounceTimer is non-nil only while a debounce window is
		// active. We use a one-shot timer rather than a ticker so an
		// idle stream doesn't churn select cases waiting on a never-
		// firing channel.
		var debounceTimer *time.Timer
		debounceCh := func() <-chan time.Time {
			if debounceTimer == nil {
				return nil
			}
			return debounceTimer.C
		}
		armDebounce := func() {
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(debounceWindow)
			}
			// If already armed, leave it — the existing window will
			// still fire and capture this notify too. Resetting on
			// every notify under heavy load could starve sends.
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-closed:
				return
			case <-nudge:
				armDebounce()
			case <-debounceCh():
				debounceTimer = nil
				if err := send(false); err != nil {
					return
				}
			case <-ticker.C:
				if err := send(false); err != nil {
					return
				}
			}
		}
	}
}

// keep context import live for downstream refactors that call ctx-aware
// helpers from this file.
var _ = context.Background

// upgrader is the WebSocket upgrade used by /live. The dashboard
// package keeps it local now that the /events handler — the other
// historical user — has moved to trace.MountDashboard with its own
// upgrader.
// Same-origin by default — the live snapshot carries the app topology and
// cached auth identities. See httpx.CheckWebSocketOrigin.
var upgrader = websocket.Upgrader{CheckOrigin: httpx.CheckWebSocketOrigin}
