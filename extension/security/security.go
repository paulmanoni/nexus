// Package security is the dashboard + per-route surface for nexus's two
// web-security defenses — security response headers and CSRF.
//
// GLOBAL enforcement does NOT live here. The framework applies security
// headers by default and enables CSRF from [runtime.middleware.security]
// in nexus.toml (see nexus.Config's Middleware.Security field) — no Go
// required, and no risk of the middleware being installed twice. This
// package adds the two things the core path can't:
//
//  1. Plugin() — a dashboard "Security" tab showing what's active.
//  2. NewHeadersMiddleware / NewCSRFMiddleware — per-route bundles for an
//     app that mixes cookie-authenticated and stateless-token routes.
//
// The header/CSRF logic itself lives in middleware/secure, shared with
// the framework core. The config types below are aliases of that package.
//
// CSRF uses the double-submit-cookie pattern with the same cookie/header
// names (csrftoken / X-CSRFToken) the generated client SDK already uses,
// so an existing frontend needs no change.
package security

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/middleware/secure"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// The config types are aliases of the shared secure package, so callers
// import only this package while the implementation stays single-sourced.
type (
	HeadersConfig = secure.HeadersConfig
	HSTSConfig    = secure.HSTSConfig
	CSRFConfig    = secure.CSRFConfig
)

// securityStatusKey mirrors the framework's App.Value key where
// installSecurity records the active posture. Kept in sync with
// app_security.go in package nexus.
const securityStatusKey = "nexus.security.status"

// Plugin adds a dashboard "Security" tab that reports whether the
// built-in headers / CSRF middleware are active. It installs no global
// middleware — configure that via [runtime.middleware.security] in
// nexus.toml (or Config.Middleware.Security). Load this only for the
// dashboard surface.
func Plugin() nexus.Option {
	return extension.Use(extension.Plugin{
		Name:    "security",
		Version: "0.1.0",
		Icon:    "shield",
		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{ID: "security", Label: "Security", Icon: "shield"},
			Routes: []extension.Route{
				{Method: "GET", Path: "/status", Handler: handleStatus},
			},
		},
	})
}

// NewHeadersMiddleware returns the security-headers middleware as a
// per-route bundle, for a subset of routes needing a different header
// policy than the global one.
func NewHeadersMiddleware(cfg HeadersConfig) middleware.Middleware {
	secure.ApplyHeaderDefaults(&cfg)
	return middleware.Middleware{
		Name:        "security-headers",
		Description: "Security response headers",
		Kind:        middleware.KindBuiltin,
		Gin:         secure.HeadersHandler(&cfg),
	}
}

// NewCSRFMiddleware returns the CSRF check as a per-route bundle. Use
// when only some routes are cookie-authenticated and the rest are a
// stateless token API you'd rather not touch. A token size below the
// safe minimum yields an always-500 guard so the misconfig is loud.
func NewCSRFMiddleware(cfg CSRFConfig) middleware.Middleware {
	secure.ApplyCSRFDefaults(&cfg)
	if err := secure.ValidateCSRF(&cfg); err != nil {
		msg := "security: CSRF misconfigured — " + err.Error()
		return middleware.Middleware{
			Name:        "csrf",
			Description: "CSRF double-submit check",
			Kind:        middleware.KindBuiltin,
			Gin: func(c *httpx.Ctx) {
				c.AbortWithStatusJSON(http.StatusInternalServerError, httpx.H{"error": msg})
			},
		}
	}
	return middleware.Middleware{
		Name:        "csrf",
		Description: "CSRF double-submit check",
		Kind:        middleware.KindBuiltin,
		Gin:         secure.CSRFHandler(&cfg),
	}
}

// handleStatus renders the active security posture from the value the
// framework core stashed at boot. Falls back to the documented defaults
// (headers on, CSRF off) when the app predates the stash.
func handleStatus(c *httpx.Ctx) {
	status := map[string]any{"headers": true, "csrf": false}
	if app, ok := nexus.AppFromGin(c); ok && app != nil {
		if v, ok := app.Value(securityStatusKey); ok {
			if m, ok := v.(map[string]any); ok {
				status = m
			}
		}
	}
	c.JSON(http.StatusOK, httpx.H{
		"headers": status["headers"],
		"csrf":    status["csrf"],
	})
}
