package cors

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// handlePolicy answers GET /__nexus/cors/policy. Returns the
// effective policy (after manifest merge + defaults) so operators
// can confirm what the binary is actually serving. Read-only.
func (s *pluginState) handlePolicy(c *httpx.Ctx) {
	c.JSON(http.StatusOK, httpx.H{
		"allowOrigins":     s.cfg.AllowOrigins,
		"allowMethods":     s.cfg.AllowMethods,
		"allowHeaders":     s.cfg.AllowHeaders,
		"exposeHeaders":    s.cfg.ExposeHeaders,
		"allowCredentials": s.cfg.AllowCredentials,
		"maxAge":           s.cfg.MaxAge,
		"disabled":         s.cfg.Disabled,
		"hasFunc":          s.cfg.AllowOriginFunc != nil,
	})
}

// handleStatus answers GET /__nexus/cors/status. Currently a small
// "is the middleware live?" check; future versions can fold in
// counters (preflight count, rejection count) if those become
// useful for ops.
func (s *pluginState) handleStatus(c *httpx.Ctx) {
	c.JSON(http.StatusOK, httpx.H{
		"started":      s.app != nil,
		"disabled":     s.cfg.Disabled,
		"originCount":  len(s.cfg.AllowOrigins),
		"funcOverride": s.cfg.AllowOriginFunc != nil,
	})
}
