package gql

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MountDashboard mounts the GraphQL introspection surface onto the
// supplied dashboard router group:
//
//	GET /graphql/cache → per-mount DocumentCache counters
//
// Always mounted (so the dashboard URL is stable), even when no
// caches are registered — in that case the response carries an
// empty list. Cheap snapshot; safe to poll.
func MountDashboard(g *gin.RouterGroup, r *StatsRegistry) {
	g.GET("/graphql/cache", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"mounts": r.Snapshot()})
	})
}
