package cron

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// MountDashboard mounts the cron introspection + control surface onto
// the supplied dashboard router group:
//
//	GET  /crons                   → snapshot of every job
//	POST /crons/:name/trigger     → run a job immediately (manual tick)
//	POST /crons/:name/pause       → pause scheduled ticks (manual Trigger still works)
//	POST /crons/:name/resume      → resume scheduled ticks
//
// Called by the dashboard package — keeps the routes declared in the
// package that owns the Scheduler type.
func MountDashboard(g httpx.Group, sched *Scheduler) {
	g.GET("/crons", func(c *httpx.Ctx) {
		c.JSON(http.StatusOK, httpx.H{"crons": sched.Snapshots()})
	})
	g.POST("/crons/:name/trigger", func(c *httpx.Ctx) {
		if !sched.Trigger(c.Param("name")) {
			c.JSON(http.StatusNotFound, httpx.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusAccepted, httpx.H{"ok": true})
	})
	g.POST("/crons/:name/pause", func(c *httpx.Ctx) {
		if !sched.SetPaused(c.Param("name"), true) {
			c.JSON(http.StatusNotFound, httpx.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, httpx.H{"paused": true})
	})
	g.POST("/crons/:name/resume", func(c *httpx.Ctx) {
		if !sched.SetPaused(c.Param("name"), false) {
			c.JSON(http.StatusNotFound, httpx.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, httpx.H{"paused": false})
	})
}
