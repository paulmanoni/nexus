package registry

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// MountDashboard mounts the registry introspection surface onto the
// supplied dashboard router group:
//
//	GET /endpoints    → {services, endpoints}
//	GET /resources    → {resources}
//	GET /workers      → {workers}
//	GET /middlewares  → {middlewares, global}
//
// Called by the dashboard package — keeps the routes declared in the
// package that owns the data so the dashboard stays a thin
// orchestrator.
func MountDashboard(g *gin.RouterGroup, reg *Registry) {
	g.GET("/endpoints", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"services":  reg.Services(),
			"endpoints": reg.Endpoints(),
		})
	})
	g.GET("/resources", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"resources": reg.Resources()})
	})
	g.GET("/workers", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"workers": reg.Workers()})
	})
	g.GET("/middlewares", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"middlewares": reg.Middlewares(),
			"global":      reg.GlobalMiddlewares(),
		})
	})
}