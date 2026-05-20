package peer

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// PeerStatus is the wire shape served at GET /__nexus/peer/list —
// one entry per declared peer with everything the dashboard's
// Peers tab needs to render a row: identity, URL, current health,
// and the in-flight call count (derived from the semaphore's
// fullness).
//
// Last error / latency stats are intentionally TODO for the first
// dashboard pass; they need a rolling-window structure the
// peerConn doesn't carry today. Adding them is a backfill on the
// peerConn struct, not a dashboard-side change.
type PeerStatus struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Healthy   bool   `json:"healthy"`
	InFlight  int    `json:"in_flight"` // calls currently holding a semaphore slot
	SemBudget int    `json:"sem_budget"`
	SchemaCached bool `json:"schema_cached"`
	LastSeen  string `json:"last_seen,omitempty"` // RFC3339; "" before any probe
}

// snapshotPeers walks the registry's peer map and produces one
// PeerStatus per peer. Snapshot semantics: each row's Healthy /
// InFlight values reflect a single moment in time; the dashboard
// frontend polls (or subscribes via live updates) for refreshes.
func snapshotPeers(r *Registry) []PeerStatus {
	out := make([]PeerStatus, 0, len(r.peers))
	for name, pc := range r.peers {
		row := PeerStatus{
			Name:      name,
			URL:       pc.url,
			Healthy:   pc.ready.Load(),
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
func handlePeerList(r *Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
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
func handlePeerSchema(r *Registry) gin.HandlerFunc {
	return func(c *gin.Context) {
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
