package dashboard

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/cron"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/ratelimit"
	"github.com/paulmanoni/nexus/registry"
)

// snapshotInterval is the cadence at which the live socket emits a fresh
// state snapshot. 2s is fast enough that the dashboard feels live but slow
// enough that a 50-endpoint app's payload (~5-10 KB) costs nothing.
const snapshotInterval = 2 * time.Second

// liveSnapshot bundles every source the dashboard renders so a single WS
// frame replaces the old (endpoints + resources + workers + stats + crons +
// ratelimits) poll fan-out. Optional subsystems (ms / sched / rl) emit nil
// fields — `omitempty` keeps the payload tight.
type liveSnapshot struct {
	Kind       string                  `json:"kind"` // always "snapshot"
	TS         time.Time               `json:"ts"`
	Services   []registry.Service      `json:"services,omitempty"`
	Endpoints  []registry.Endpoint     `json:"endpoints,omitempty"`
	Resources  []registry.ResourceSnapshot `json:"resources,omitempty"`
	Workers    []registry.Worker       `json:"workers,omitempty"`
	Stats      []metrics.EndpointStats `json:"stats,omitempty"`
	Crons      []cron.Snapshot         `json:"crons,omitempty"`
	RateLimits []ratelimit.Record      `json:"ratelimits,omitempty"`
}

// streamLive is the WS handler at /__nexus/live. Sends an initial snapshot
// on connect, then a fresh snapshot every snapshotInterval. The writer
// never blocks indefinitely — a missed write deadline closes the conn so
// the client's auto-reconnect resumes cleanly.
//
// Optional subsystems are tolerated: pass nil for ms / sched / rl when
// they're not wired and those fields drop out of the payload.
func streamLive(reg *registry.Registry, ms metrics.Store, sched *cron.Scheduler, rl ratelimit.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		ctx := c.Request.Context()
		send := func() error {
			snap := liveSnapshot{
				Kind:      "snapshot",
				TS:        time.Now(),
				Services:  reg.Services(),
				Endpoints: reg.Endpoints(),
				Resources: reg.Resources(),
				Workers:   reg.Workers(),
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
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			return conn.WriteJSON(snap)
		}

		// Initial snapshot — the client treats first frame as "fully loaded"
		// so the dashboard renders before the first tick fires.
		if err := send(); err != nil {
			return
		}

		// Detect client close in a separate goroutine. NextReader blocks
		// until the peer sends a frame or closes the conn; on close it
		// returns an error and we signal the loop to exit.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			for {
				if _, _, err := conn.NextReader(); err != nil {
					return
				}
			}
		}()

		ticker := time.NewTicker(snapshotInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := send(); err != nil {
					return
				}
			case <-closed:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

// keep context import live for downstream refactors that call ctx-aware
// helpers from this file.
var _ = context.Background