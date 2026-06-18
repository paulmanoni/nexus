package gql

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// MountDashboard mounts the GraphQL introspection surface onto the
// supplied dashboard router group:
//
//	GET /graphql/cache → per-mount DocumentCache counters
//
// Always mounted (so the dashboard URL is stable), even when no
// caches are registered — in that case the response carries an
// empty list. Cheap snapshot; safe to poll.
func MountDashboard(g httpx.Group, r *StatsRegistry) {
	g.GET("/graphql/cache", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"mounts": r.Snapshot()})
	})
}
