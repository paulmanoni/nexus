package cron

import (
	"net/http"

	"github.com/gin-gonic/gin"
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
func MountDashboard(g *gin.RouterGroup, sched *Scheduler) {
	g.GET("/crons", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"crons": sched.Snapshots()})
	})
	g.POST("/crons/:name/trigger", func(c *gin.Context) {
		if !sched.Trigger(c.Param("name")) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"ok": true})
	})
	g.POST("/crons/:name/pause", func(c *gin.Context) {
		if !sched.SetPaused(c.Param("name"), true) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"paused": true})
	})
	g.POST("/crons/:name/resume", func(c *gin.Context) {
		if !sched.SetPaused(c.Param("name"), false) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown cron"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"paused": false})
	})
}