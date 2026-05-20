package peer

import (
	"context"
	"net/http"
	"time"
)

// proberInterval is how often each per-peer prober hits the
// /__peer/health endpoint. 10s balances "fail-fast on outages" against
// "don't drown the peer in idle health pings" — a 1-minute outage
// gets caught within 10s, which is well under any reasonable LB
// drain timeout. Tunable via Config.ProberInterval (TODO; not yet
// exposed because the value isn't load-bearing for any current
// deployment).
const proberInterval = 10 * time.Second

// proberTimeout caps an individual probe. 2s is tight enough to
// pick up "the peer is up but its event loop is stuck" failures
// alongside the "peer is unreachable" case. Anything longer would
// inflate the time between proberInterval ticks under load.
const proberTimeout = 2 * time.Second

// startProbers spawns one goroutine per declared peer, each
// hitting /__peer/health every proberInterval and flipping
// peerConn.ready accordingly. Returns a cancel func the Lifecycle
// OnShutdown hook calls to stop every prober cleanly on app stop.
//
// Behavior:
//   - First probe runs immediately so a Registry that's "ready"
//     by default doesn't sit on stale optimistic state until the
//     first interval elapses; if a peer is down at boot, the
//     first round of Calls will fast-fail correctly.
//   - On failure: ready=false, schemas cache for the peer is
//     reset so the next recovery re-fetches the schema (peer
//     may have rolled out a new version while we couldn't see it).
//   - On recovery: ready=true. No extra book-keeping; the schema
//     cache repopulates lazily on the next Call.
//   - On ctx cancel: every goroutine exits within one interval.
func startProbers(ctx context.Context, r *Registry) {
	for _, pc := range r.peers {
		go proberLoop(ctx, r, pc)
	}
}

func proberLoop(ctx context.Context, r *Registry, pc *peerConn) {
	// Initial probe before the ticker so we close the "optimistic
	// ready until the first failure" gap. Without this, a peer
	// that's down at boot would sit at ready=true for up to
	// proberInterval, and the IsHealthy fast-fail path would
	// stay unprimed.
	probeOnce(ctx, r, pc)

	t := time.NewTicker(proberInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probeOnce(ctx, r, pc)
		}
	}
}

// probeOnce sends a single GET to /__peer/health with a tight
// timeout, then updates ready + the schema cache based on the
// outcome. Errors and non-200 responses both count as "down";
// any 2xx counts as "up".
func probeOnce(parent context.Context, r *Registry, pc *peerConn) {
	ctx, cancel := context.WithTimeout(parent, proberTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pc.url+"/__peer/health", nil)
	if err != nil {
		// Bad URL at construction time — unrecoverable; mark
		// down so the operator notices and rerolls config. The
		// fix is in Config.Peers, not in this loop.
		flipReady(r, pc, false)
		return
	}
	resp, err := pc.httpClient.Do(req)
	if err != nil {
		flipReady(r, pc, false)
		return
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	flipReady(r, pc, ok)
}

// flipReady applies an atomic transition + side effects. The
// schema-cache reset only fires on a true→false transition (a
// peer that's been down then up means the version we cached may
// have changed; fresh fetch on the next Call picks up the new
// schema). Steady-state transitions skip the reset.
func flipReady(r *Registry, pc *peerConn, up bool) {
	prev := pc.ready.Swap(up)
	if prev != up {
		if !up {
			r.schemas.reset(pc.name)
		}
	}
}
