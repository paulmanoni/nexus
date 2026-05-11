package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// dashboardListHandler builds the GET /__nexus/auth handler. Returns
// the redacted identity snapshot + caching-enabled flag. Tokens never
// leave the cache — Identities() already truncates to an 8-char prefix.
//
// Exposed (rather than mounted inline) so auth.Module can hand it to
// extension.Use as a Dashboard.Route — that's the seam built-in plugins
// share with custom ones.
func dashboardListHandler(m *Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"identities":     m.Identities(),
			"cachingEnabled": m.state.cache != nil,
		})
	}
}

// dashboardInvalidateHandler builds the POST /__nexus/auth/invalidate
// handler. Accepts {"id": "user-x"} or {"token": "..."}; replies with
// {"dropped": N}.
func dashboardInvalidateHandler(m *Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
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
			dropped++ // best-effort: callers typically pass one or the other.
		}
		c.JSON(http.StatusOK, gin.H{"dropped": dropped})
	}
}