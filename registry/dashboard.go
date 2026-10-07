package registry

import (
	"net/http"

	"github.com/paulmanoni/nexus/v2/httpx"
)

// MountDashboard mounts the registry introspection surface onto the
// supplied dashboard router group:
//
//	GET /endpoints    → {services, endpoints}
//	GET /routes       → {routes}: every REST route with its name
//	GET /resources    → {resources}
//	GET /workers      → {workers}
//	GET /middlewares  → {middlewares, global}
//
// Called by the dashboard package — keeps the routes declared in the
// package that owns the data so the dashboard stays a thin
// orchestrator.
func MountDashboard(g httpx.Group, reg *Registry) {
	g.GET("/endpoints", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{
			"services":  reg.Services(),
			"endpoints": reg.VisibleEndpoints(),
		})
	})
	g.GET("/routes", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"routes": reg.Routes()})
	})
	g.GET("/resources", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"resources": reg.Resources()})
	})
	g.GET("/workers", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"workers": reg.Workers()})
	})
	g.GET("/middlewares", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{
			"middlewares": reg.Middlewares(),
			"global":      reg.GlobalMiddlewares(),
		})
	})
}
