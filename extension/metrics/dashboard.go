package metrics

import (
	"net/http"

	"github.com/gin-gonic/gin"
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
func MountDashboard(g *gin.RouterGroup, store Store) {
	g.GET("/stats", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"stats": store.Snapshot()})
	})
	g.GET("/stats/errors", func(c *gin.Context) {
		s := c.Query("service")
		o := c.Query("op")
		key := s + "." + o
		c.JSON(http.StatusOK, gin.H{
			"key":    key,
			"events": store.Errors(key),
		})
	})
}