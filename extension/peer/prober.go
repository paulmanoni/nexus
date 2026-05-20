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

// probeOnce probes every target under the named peer in parallel
// (one HTTP GET per target). Per-target ready flags are updated
// independently — one replica being down doesn't take the whole
// peer offline as long as another is reachable.
//
// The schema cache is reset on a ready→down transition at the
// PEER level, not per target: any healthy target serves the same
// schema, so we only need to invalidate when EVERY target
// transitions away from ready.
func probeOnce(parent context.Context, r *Registry, pc *peerConn) {
	targets := pc.snapshotTargets()
	if len(targets) == 0 {
		return
	}
	anyWasReady := pc.anyReady()
	for _, tgt := range targets {
		probeTarget(parent, pc, tgt)
	}
	anyIsReady := pc.anyReady()
	// Peer-level ready→down transition (every target now down):
	// invalidate the schema cache so recovery picks up a
	// possibly-rolled-out new schema on the next Call.
	if anyWasReady && !anyIsReady {
		r.schemas.reset(pc.name)
	}
}

// probeTarget sends a single GET to one target's /__peer/health.
// Updates the target's ready flag based on the outcome; doesn't
// touch the schema cache (that's a peer-level concern, handled
// by probeOnce after the per-target loop completes).
func probeTarget(parent context.Context, pc *peerConn, tgt *peerTarget) {
	ctx, cancel := context.WithTimeout(parent, proberTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tgt.url+"/__peer/health", nil)
	if err != nil {
		tgt.ready.Store(false)
		return
	}
	resp, err := pc.httpClient.Do(req)
	if err != nil {
		tgt.ready.Store(false)
		return
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	tgt.ready.Store(ok)
}

// flipReady is the legacy single-target shim kept for tests
// written against the rc3 shape. New code should call
// probeOnce / probeTarget directly. Flips every target on the
// peer to up/down and applies the same schema-cache reset on
// peer-level down transitions that probeOnce does.
func flipReady(r *Registry, pc *peerConn, up bool) {
	wasReady := pc.anyReady()
	for _, tgt := range pc.snapshotTargets() {
		tgt.ready.Store(up)
	}
	if wasReady && !up {
		r.schemas.reset(pc.name)
	}
}
