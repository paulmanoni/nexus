package peer

import (
	"net/http"
	"time"

	"github.com/paulmanoni/nexus/httpx"
)

// PeerStatus is the wire shape served at GET /__nexus/peer/list —
// one entry per declared peer with everything the dashboard's
// Peers tab needs to render a row.
//
// Targets is the per-replica detail: SRV-resolved peers may have
// 1..N targets; URL-only peers always have exactly one. The
// Healthy field at the peer level is true when at least one
// target is currently reachable (matches Registry.IsHealthy).
type PeerStatus struct {
	Name         string         `json:"name"`
	Healthy      bool           `json:"healthy"`   // true when at least one target is up
	Targets      []TargetStatus `json:"targets"`   // per-replica detail; always >= 1 in steady state
	InFlight     int            `json:"in_flight"` // calls currently holding a semaphore slot
	SemBudget    int            `json:"sem_budget"`
	SchemaCached bool           `json:"schema_cached"`
}

// TargetStatus is one row in PeerStatus.Targets — one URL +
// current ready flag. Per-target latency / error history is
// still TODO (rolling window backfill on peerTarget); the
// dashboard renders "online/offline" only for now.
type TargetStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
}

// snapshotPeers walks the registry's peer map and produces one
// PeerStatus per peer. Snapshot semantics: each row's Healthy /
// InFlight / Targets values reflect a single moment in time;
// the dashboard frontend polls (or subscribes via live updates)
// for refreshes.
func snapshotPeers(r *Registry) []PeerStatus {
	out := make([]PeerStatus, 0, len(r.peers))
	for name, pc := range r.peers {
		targets := pc.snapshotTargets()
		rows := make([]TargetStatus, 0, len(targets))
		anyReady := false
		for _, t := range targets {
			ready := t.ready.Load()
			rows = append(rows, TargetStatus{URL: t.url, Healthy: ready})
			if ready {
				anyReady = true
			}
		}
		row := PeerStatus{
			Name:      name,
			Healthy:   anyReady,
			Targets:   rows,
			InFlight:  len(pc.sem),
			SemBudget: cap(pc.sem),
		}
		// Schema cache state — useful for diagnosing "drift
		// check didn't fire" surprises.
		r.schemas.mu.Lock()
		_, row.SchemaCached = r.schemas.schemas[name]
		r.schemas.mu.Unlock()
		out = append(out, row)
	}
	return out
}

// handlePeerList returns the JSON shape the dashboard's Peers tab
// fetches. Mounted at GET /__nexus/peer/list by the extension.Use
// Routes wiring in peer.go.
func handlePeerList(r *Registry) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, map[string]any{
			"identity":  r.identity,
			"peers":     snapshotPeers(r),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// handlePeerSchema returns the schema the framework has cached
// for the named peer (or an empty body when no schema has been
// fetched yet). Useful for "what does my peer actually expose?"
// inspection right from the dashboard.
func handlePeerSchema(r *Registry) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		name := c.Param("name")
		r.schemas.mu.Lock()
		schema, ok := r.schemas.schemas[name]
		r.schemas.mu.Unlock()
		if !ok {
			c.JSON(http.StatusNotFound, map[string]string{
				"error": "no cached schema for peer " + name +
					" — make at least one Call to populate the cache",
			})
			return
		}
		c.JSON(http.StatusOK, schema)
	}
}
