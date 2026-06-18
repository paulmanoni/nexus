package metrics

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// MountDashboard mounts the metrics introspection surface onto the
// supplied dashboard router group:
//
//	GET /stats              → counter snapshot for every endpoint
//	GET /stats/errors?service=&op= → per-op error ring buffer
//
// service + op live in query params because op names contain slashes
// (REST: "<METHOD> <path>") that gin's path-param matcher can't
// capture across segment boundaries.
//
// /stats/errors is lazy-loaded by the dashboard when an operator
// opens the error dialog for a specific endpoint — keeps /stats lean
// even when RecentErrorsCap is in the thousands.
func MountDashboard(g httpx.Group, store Store) {
	g.GET("/stats", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"stats": store.Snapshot()})
	})
	g.GET("/stats/errors", func(c *httpx.Ctx) {
		s := c.Query("service")
		o := c.Query("op")
		key := s + "." + o
		c.JSON(http.StatusOK, httpx.H{
			"key":    key,
			"events": store.Errors(key),
		})
	})
}
