package ratelimit

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// MountDashboard mounts the rate-limit introspection + override
// surface onto the supplied dashboard router group:
//
//	GET    /ratelimits                    → snapshot of every key
//	POST   /ratelimits?service=...&op=... → override limit live
//	DELETE /ratelimits?service=...&op=... → reset to declared baseline
//
// Called by the dashboard package at mount time — keeps the routes
// declared in the package that owns the data (and the Store type), so
// the dashboard package stays thin.
//
// service + op live in QUERY params (not :path/:params) because REST
// op names are "<METHOD> <path>" and contain slashes that gin's path-
// param matcher can't capture across segment boundaries. Query params
// handle URL-encoding cleanly.
//
// The key format is "<service>.<op>" — matches what auto-mount
// registers at boot so dashboard and store talk the same dialect.
func MountDashboard(g httpx.Group, store Store) {
	g.GET("/ratelimits", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"limits": store.Snapshot(c.Request.Context())})
	})
	g.POST("/ratelimits", func(c *httpx.Ctx) {
		var body Limit
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, httpx.H{"error": err.Error()})
			return
		}
		key := c.Query("service") + "." + c.Query("op")
		rec, err := store.Configure(c.Request.Context(), key, body)
		if err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, rec)
	})
	g.DELETE("/ratelimits", func(c *httpx.Ctx) {
		key := c.Query("service") + "." + c.Query("op")
		if err := store.Reset(c.Request.Context(), key); err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, httpx.H{"ok": true})
	})
}
