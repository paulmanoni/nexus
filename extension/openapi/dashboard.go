package openapi

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/paulmanoni/nexus/client"
)

// handleSpecJSON serves the OpenAPI document as JSON. The default
// Content-Type ("application/json") satisfies every tool we've
// surveyed; some prefer "application/vnd.oai.openapi+json" but
// nothing demands it.
func (s *pluginState) handleSpecJSON(c *gin.Context) {
	doc := s.currentSpec(c)
	c.JSON(http.StatusOK, doc)
}

// handleSpecYAML serves the same document as YAML. SDK generators
// accept both; humans tend to read YAML more easily, so this is here
// for the "open the URL in a browser tab" path.
func (s *pluginState) handleSpecYAML(c *gin.Context) {
	doc := s.currentSpec(c)
	out, err := yaml.Marshal(doc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", out)
}

// handleUI serves Swagger UI as a single HTML page. CDN-hosted —
// the page loads swagger-ui-dist from unpkg, with the spec URL
// pointed at our own /spec.json. Pinning a specific version means
// no surprise UI changes when unpkg's "latest" moves.
//
// Why HTML inline vs an embedded asset bundle: keeps the plugin
// dependency-free. Operators on a strict CSP can override the page
// by mounting their own handler at /__nexus/openapi/ui — the
// framework's last-route-wins means a later registration replaces
// this one.
func (s *pluginState) handleUI(c *gin.Context) {
	specPath := s.specURL()
	html := fmt.Sprintf(swaggerUIHTML, s.cfg.SwaggerUIVersion, s.cfg.Title, s.cfg.SwaggerUIVersion, s.cfg.SwaggerUIVersion, specPath)
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

// currentSpec is the lazy generator: reads the live client manifest
// every request, runs buildDocument, returns the result. Cheap enough
// for any realistic dashboard usage (sub-millisecond on small apps,
// a few ms on the largest registries we've measured).
//
// Servers default — when Config.Servers is empty, synthesize one
// entry from the current request's host so a hit to
// /__nexus/openapi/spec.json from a browser still produces a usable
// "Try it out" URL in Swagger UI. Explicit Config.Servers overrides
// this; production deployments should set it to their canonical URL.
func (s *pluginState) currentSpec(c *gin.Context) Document {
	mf := s.fetchManifest()
	doc := s.buildDocument(mf)
	if len(doc.Servers) == 0 {
		doc.Servers = []Server{{
			URL: scheme(c) + "://" + c.Request.Host,
		}}
	}
	return doc
}

// fetchManifest reads the live SDK manifest from the app's client
// handler. When the client SDK isn't wired (Config.Client.Enabled
// false), there's no manifest — return an empty one so the spec
// renders with a valid (if empty) Paths block instead of crashing.
//
// This is the "the plugin is loaded but the framework's typed
// registry projection is off" case. The OpenAPI spec is still
// generated; it just lists no endpoints, with a clear message in
// info.description.
func (s *pluginState) fetchManifest() client.Manifest {
	if s.app == nil {
		return emptyManifest()
	}
	h := s.app.ClientHandler()
	if h == nil {
		return emptyManifest()
	}
	return h.Manifest()
}

// emptyManifest produces a stub for the "no SDK wired" case so the
// spec still renders cleanly (empty Paths, no Refs). Useful during
// integration so operators don't see a 500 when they hit /openapi.json
// before remembering to flip Config.Client.Enabled.
func emptyManifest() client.Manifest {
	return client.Manifest{
		Version: client.SchemaVersion,
	}
}

// specURL builds the URL the UI page should load the spec from.
// When PublicRoot is on we prefer /openapi.json (the conventional
// path); otherwise we use the namespaced one. Either way it's
// host-relative so the UI works behind proxies.
func (s *pluginState) specURL() string {
	if s.publicRoot() {
		return "/openapi.json"
	}
	return "/__nexus/openapi/spec.json"
}

// scheme detects whether the request came in over TLS — needed for
// the synthesized server URL in currentSpec(). Honors the standard
// proxy headers so a deployment behind an LB still gets "https".
func scheme(c *gin.Context) string {
	if c.Request.TLS != nil {
		return "https"
	}
	if h := c.GetHeader("X-Forwarded-Proto"); h != "" {
		return h
	}
	return "http"
}

// swaggerUIHTML is the format string for the /ui page. Five %s
// placeholders, in order:
//
//	1. swagger-ui-dist CSS link version
//	2. document title
//	3. swagger-ui-dist bundle JS version
//	4. swagger-ui-dist standalone preset version
//	5. spec URL the bundle should load
//
// Stylistically minimal — no theming, no auth UI overrides. Teams
// with stricter design requirements should mount their own /ui
// handler; the framework lets later registrations win.
const swaggerUIHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@%s/swagger-ui.css" />
  <title>%s — API docs</title>
  <style>
    body { margin: 0; }
    .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@%s/swagger-ui-bundle.js" crossorigin></script>
  <script src="https://unpkg.com/swagger-ui-dist@%s/swagger-ui-standalone-preset.js" crossorigin></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: %q,
        dom_id: '#swagger-ui',
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: 'StandaloneLayout',
        deepLinking: true,
      });
    };
  </script>
</body>
</html>
`
