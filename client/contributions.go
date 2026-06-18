package client

import (
	"net/http"

	"github.com/paulmanoni/nexus/httpx"
)

// ContributionsResponse is the wire format served at
// GET <path>/contributions.json. The CLI's frontend codegen merges
// these files into the renderer's output tree alongside the per-op
// typed exports — phase 3's escape hatch for plugin-specific TS
// (auth's useAuth composable, oauth2's flow helpers) that the
// transport-neutral renderer doesn't know how to emit.
//
// Body is plain TS source as a JSON string. Binary contributions
// aren't supported in v1 — TS/JS contributors are the entire
// audience and base64 padding would just bloat the wire.
type ContributionsResponse struct {
	Version   string                  `json:"version"`
	Framework string                  `json:"framework,omitempty"`
	Plugins   []ContributionPluginRec `json:"plugins,omitempty"`
}

// ContributionPluginRec groups one plugin's files under its name.
// Empty Files entries get omitted from the wire — a plugin that
// emits nothing for the requested framework leaves no trace.
type ContributionPluginRec struct {
	Name  string                `json:"name"`
	Files []ContributionFileRec `json:"files,omitempty"`
}

// ContributionFileRec is one rendered artifact. Path is forward-slash
// relative to whatever OutDir the consumer picks; Body is the TS/JS
// source ready to land on disk.
type ContributionFileRec struct {
	Path string `json:"path"`
	Body string `json:"body"`
}

// ContributionsBuilder is the closure Mount accepts to make
// contributions HTTP-reachable. Takes the framework requested via
// the ?framework= query parameter and returns the merged response.
// Returning an error produces a 500 with the error text — the CLI
// treats this as a hard failure (a misbehaving contributor blocks
// codegen; better to fail loudly than ship a broken tree).
//
// nil signals "no contributions route" — Mount skips registration
// entirely. Legacy callers (nexus.ClientUse, the old Config.Client
// auto-mount) pass nil so the route is opt-in.
type ContributionsBuilder func(framework string) (ContributionsResponse, error)

// contributionsHandler returns the gin handler for the contributions
// route. Kept distinct from the Mount registration so tests can
// drive the handler directly without spinning a full engine.
func contributionsHandler(build ContributionsBuilder) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		framework := c.Query("framework")
		resp, err := build(framework)
		if err != nil {
			c.JSON(http.StatusInternalServerError, httpx.H{"error": err.Error()})
			return
		}
		if resp.Version == "" {
			resp.Version = SchemaVersion
		}
		if resp.Framework == "" {
			resp.Framework = framework
		}
		c.JSON(http.StatusOK, resp)
	}
}
