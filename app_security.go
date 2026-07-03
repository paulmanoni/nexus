package nexus

import (
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/middleware/secure"
)

// securityStatusKey is the extValues key under which installSecurity
// stashes the resolved security posture. The extension/security
// dashboard tab reads it via App.Value so it can show what's actually
// active without re-deriving config.
const securityStatusKey = "nexus.security.status"

// installSecurity wires the built-in security middleware from
// Config.Middleware.Security. Headers are on unless explicitly disabled
// (so a nil config still hardens the app); CSRF is opt-in. It records a
// status map for the dashboard either way.
func (a *App) installSecurity(sc *SecurityConfig) {
	headersOn := sc == nil || !sc.DisableHeaders
	csrfOn := sc != nil && sc.EnableCSRF

	if headersOn {
		hc := secure.HeadersConfig{}
		if sc != nil {
			hc.FrameOptions = sc.FrameOptions
			hc.ReferrerPolicy = sc.ReferrerPolicy
			hc.ContentSecurityPolicy = sc.CSP
			if sc.HSTSMaxAge > 0 {
				hc.HSTS = &secure.HSTSConfig{MaxAge: sc.HSTSMaxAge}
			}
		}
		secure.ApplyHeaderDefaults(&hc)
		a.engine.Use(secure.HeadersHandler(&hc))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "security-headers",
			Kind:        middleware.KindBuiltin,
			Description: "Security response headers (built-in)",
		})
		a.registry.RegisterGlobalMiddleware("security-headers")
	}

	if csrfOn {
		cc := secure.CSRFConfig{}
		if sc != nil {
			cc.CookieSecure = sc.CSRFCookieSecure
		}
		secure.ApplyCSRFDefaults(&cc)
		a.engine.Use(secure.CSRFHandler(&cc))
		a.registry.RegisterMiddleware(middleware.Info{
			Name:        "csrf",
			Kind:        middleware.KindBuiltin,
			Description: "CSRF double-submit check (built-in)",
		})
		a.registry.RegisterGlobalMiddleware("csrf")
	}

	a.SetValue(securityStatusKey, map[string]any{
		"headers": headersOn,
		"csrf":    csrfOn,
	})
}
