// Package dashboard mounts the nexus introspection surface under /__nexus.
// Ships a Vue dashboard (embedded from ui/dist), a JSON registry listing, and
// a WebSocket event stream.
package dashboard

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

const Prefix = "/__nexus"

// Config is what the dashboard client fetches at startup. Extend with more
// fields (version, environment, etc.) as needed.
type Config struct {
	Name string `json:"Name"`
}

// Mount attaches:
//
//	GET  /__nexus/config      -> Config JSON
//	GET  /__nexus/endpoints   -> services + endpoints from the registry
//	GET  /__nexus/events      -> WebSocket: backlog (since=N) then live trace events
//	GET  /__nexus/, /assets/* -> embedded Vue dashboard
//
// The events endpoint is only mounted if bus != nil.
func Mount(e *gin.Engine, reg *registry.Registry, bus *trace.Bus, cfg Config) {
	if cfg.Name == "" {
		cfg.Name = "Nexus"
	}
	g := e.Group(Prefix)
	g.GET("/config", func(c *gin.Context) {
		c.JSON(http.StatusOK, cfg)
	})
	g.GET("/endpoints", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"services":  reg.Services(),
			"endpoints": reg.Endpoints(),
		})
	})
	g.GET("/resources", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"resources": reg.Resources()})
	})
	g.GET("/middlewares", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"middlewares": reg.Middlewares()})
	})
	if bus != nil {
		g.GET("/events", streamEvents(bus))
	}
	mountUI(g)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

func streamEvents(bus *trace.Bus) gin.HandlerFunc {
	return func(c *gin.Context) {
		var since int64
		if s := c.Query("since"); s != "" {
			since, _ = strconv.ParseInt(s, 10, 64)
		}
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		backlog, ch, cancel := bus.Subscribe(since, 128)
		defer cancel()

		for _, e := range backlog {
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		}
		for e := range ch {
			if err := conn.WriteJSON(e); err != nil {
				return
			}
		}
	}
}
