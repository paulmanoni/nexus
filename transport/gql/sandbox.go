package gql

import (
	"net/http"
	"strings"

	"github.com/paulmanoni/nexus/httpx"
)

// apolloSandboxHTML is the embedded Apollo Sandbox IDE. Sandbox is the
// default GraphQL GUI nexus serves to browser visitors of the GraphQL
// path (replacing go-graph's bundled Playground). It's a single static
// page that loads the Sandbox bundle from Apollo's CDN and points it at
// the current path, so the same endpoint serves both the API (POST) and
// the IDE (GET from a browser).
//
// initialEndpoint is computed from window.location at load time —
// origin + pathname, stripped of any query string — so Sandbox issues
// its operations against whatever path this handler is mounted on
// (honoring route_prefix, per-service AtGraphQL, sub-path mounts, etc.)
// without the server having to template the URL in.
const apolloSandboxHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>GraphQL Sandbox</title>
  <style>
    html, body { margin: 0; padding: 0; height: 100%; overflow: hidden; }
    #sandbox { height: 100vh; width: 100vw; }
  </style>
</head>
<body>
  <div id="sandbox"></div>
  <script src="https://embeddable-sandbox.cdn.apollographql.com/_latest/embeddable-sandbox.umd.production.min.js"></script>
  <script>
    new window.EmbeddedSandbox({
      target: "#sandbox",
      initialEndpoint: window.location.origin + window.location.pathname,
    });
  </script>
</body>
</html>`

// apolloSandboxHandler serves the Apollo Sandbox IDE page.
func apolloSandboxHandler() httpx.HandlerFunc {
	body := []byte(apolloSandboxHTML)
	return func(c *httpx.Ctx) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write(body)
	}
}

// isBrowserIDEVisit reports whether a GET request is a human opening the
// GraphQL path in a browser (wanting the IDE) rather than a tool issuing
// a GraphQL operation over GET. A real GET query carries a ?query=
// param; a browser navigation carries an Accept: text/html preference
// and no query. Sandbox's own operations are POSTs, so they never reach
// this path.
func isBrowserIDEVisit(c *httpx.Ctx) bool {
	if c.Query("query") != "" {
		return false
	}
	return strings.Contains(c.GetHeader("Accept"), "text/html")
}
