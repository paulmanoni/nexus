package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// mountDashboardRoutes installs /__nexus/auth endpoints on the engine.
// Routes live on the engine root (not inside the dashboard's
// RouterGroup) because auth.Module runs in an fx.Invoke — after
// dashboard.Mount has already finalized its group at app construction
// time. Consequence: Config.DashboardMiddleware does NOT automatically
// apply to these routes. Apps that need them protected should put the
// same middleware bundle on nexus.Config.GlobalMiddleware so it
// covers both the dashboard group AND these auth routes.
//
// Endpoints:
//
//	GET  /__nexus/auth            → { identities: [...], cachingEnabled: bool }
//	POST /__nexus/auth/invalidate → invalidate by identity ID
//	                                body: { "id": "user-123" }
//	                                reply: { "dropped": N }
//
// Token values are never returned — Identities() already redacts to
// an 8-char prefix, so the dashboard JSON carries no raw credentials.
func mountDashboardRoutes(e *gin.Engine, m *Manager) {
	e.GET("/__nexus/auth", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"identities":     m.Identities(),
			"cachingEnabled": m.state.cache != nil,
		})
	})
	e.POST("/__nexus/auth/invalidate", func(c *gin.Context) {
		var body struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if body.ID == "" && body.Token == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id or token required"})
			return
		}
		dropped := 0
		if body.ID != "" {
			dropped += m.InvalidateByIdentity(body.ID)
		}
		if body.Token != "" {
			m.Invalidate(body.Token)
			dropped++ // Invalidate is a no-op when absent, but count
			// best-effort: callers typically pass one or the other.
		}
		c.JSON(http.StatusOK, gin.H{"dropped": dropped})
	})
}