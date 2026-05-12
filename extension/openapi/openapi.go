// Package openapi auto-generates an OpenAPI 3.1 specification from
// the framework's typed registry and serves it (plus Swagger UI)
// from a nexus app. Zero hand-written spec, always in sync with the
// running app.
//
// Wire it up next to your other modules. The plugin reads from the
// client manifest (which the framework already builds from
// reflection) and emits an OpenAPI document on demand. No build
// step, no codegen invocation.
//
//	import "github.com/paulmanoni/nexus/extension/openapi"
//
//	nexus.Run(
//	    nexus.Config{
//	        Server: nexus.ServerConfig{Addr: ":8080"},
//	        Client: client.Config{Enabled: true}, // the plugin reads from here
//	    },
//	    openapi.Plugin(openapi.Config{
//	        Title:       "Pet Shop API",
//	        Version:     "1.0.0",
//	        Description: "Catalog + orders + waitlist",
//	    }),
//	    // ... rest of the app
//	)
//
// What the plugin exposes:
//
//	GET /__nexus/openapi/spec.json   — the spec (JSON, primary)
//	GET /__nexus/openapi/spec.yaml   — same spec, YAML encoding
//	GET /__nexus/openapi/ui          — Swagger UI HTML, loads spec.json
//
// And, when Config.PublicRoot is true (the default), additional
// routes at the API root so customer-facing tooling finds them at
// the conventional paths:
//
//	GET /openapi.json
//	GET /api-docs              — Swagger UI
//
// Customers run `openapi-generator-cli` against /openapi.json and
// get a typed client SDK in any language. The same spec drives
// Postman collections, Schemathesis fuzzing, Spectral linting.
package openapi

import (
	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension"
)

// Config controls the metadata block of the generated spec and a
// handful of behavior toggles. Everything else (paths, operations,
// schemas) is derived from the live registry.
type Config struct {
	// Title is the API name that appears in info.title. Defaults to
	// "Nexus API" if empty.
	Title string

	// Version is info.version. Defaults to "1.0.0". Bump this when
	// your wire contract changes — Spectral diff in CI compares
	// successive versions of the same spec.
	Version string

	// Description is info.description. Markdown is permitted (the
	// OpenAPI spec allows CommonMark there).
	Description string

	// Contact is info.contact. Optional; empty means "omitted from
	// spec". Most clients ignore this; Stoplight + Postman surface
	// it on the docs page.
	Contact *Contact

	// License is info.license. Optional; same notes as Contact.
	License *License

	// Servers is the list of base URLs the spec advertises under
	// servers[]. When empty, the plugin synthesizes a single entry
	// from the request host at serve time so a local dev hit to
	// /__nexus/openapi/spec.json still sees "http://localhost:8080"
	// without configuration.
	Servers []Server

	// PublicRoot, when nil or *true, also mounts /openapi.json and
	// /api-docs at the API root in addition to the namespaced
	// /__nexus/openapi/ paths. Disable only if your gateway already
	// hosts these paths and you want the framework to stay out of
	// their way.
	PublicRoot *bool

	// ExcludeAdmin, when nil or *true, omits framework-owned
	// /__nexus/* paths from the spec. Customers shouldn't see admin
	// routes in the SDK they generate. Flip to *false for an
	// admin-facing variant.
	ExcludeAdmin *bool

	// IncludeGraphQL, when *true, also emits a /graphql operation
	// for GraphQL endpoints. Defaults to false because OpenAPI
	// describes REST poorly when the underlying transport is
	// GraphQL — most teams expose the GraphQL schema separately
	// via /graphql?schema or graphql-introspection. The flag is
	// here for the rare case where a customer must consume both
	// transports through one spec.
	IncludeGraphQL *bool

	// SwaggerUIVersion pins the CDN version of swagger-ui-dist
	// loaded by the /ui page. Default "5.20.1". Override only when
	// pinning to an audited release.
	SwaggerUIVersion string
}

// Contact / License / Server mirror their OpenAPI counterparts.
// Repeated here so callers can configure them in Go without dragging
// in an OpenAPI library as a public dep.
type Contact struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

type License struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// Plugin wires the OpenAPI generator into the app's plugin chain.
// Generation runs lazily at request time so a freshly-added endpoint
// shows up in /spec.json without a restart (the live registry is the
// source). On a stable production app the overhead is one map walk
// per request; for higher throughput we could cache and invalidate
// on registry.OnChange, but that's premature for the v1 size.
func Plugin(cfg Config) nexus.Option {
	applyDefaults(&cfg)
	state := &pluginState{cfg: cfg}

	return extension.Use(extension.Plugin{
		Name:    "openapi",
		Version: "0.1.0",

		// Options runs early enough to attach root-level routes
		// (/openapi.json, /api-docs) outside the /__nexus/openapi/
		// namespace via app.Engine(). Mounting through the
		// Dashboard.Routes slot alone would force everything under
		// /__nexus/openapi/, but `openapi.json` at the API root is
		// the convention every SDK tool expects.
		Options: []nexus.Option{
			nexus.Invoke(func(app *nexus.App) {
				state.app = app
				if cfg.PublicRoot == nil || *cfg.PublicRoot {
					eng := app.Engine()
					eng.GET("/openapi.json", state.handleSpecJSON)
					eng.GET("/openapi.yaml", state.handleSpecYAML)
					eng.GET("/api-docs", state.handleUI)
				}
			}),
		},

		Dashboard: &extension.Dashboard{
			Tab: &extension.Tab{
				ID:    "openapi",
				Label: "API Spec",
				Icon:  "book-open",
			},
			Routes: []extension.Route{
				{Method: "GET", Path: "/spec.json", Handler: state.handleSpecJSON},
				{Method: "GET", Path: "/spec.yaml", Handler: state.handleSpecYAML},
				{Method: "GET", Path: "/ui", Handler: state.handleUI},
			},
		},
	})
}

// pluginState holds the resolved Config + a live App reference so the
// HTTP handlers can read the registry on demand without going
// through fx for each request.
type pluginState struct {
	cfg Config
	app *nexus.App
}

// applyDefaults fills in the conventional values so a bare Config{}
// produces a usable spec.
func applyDefaults(cfg *Config) {
	if cfg.Title == "" {
		cfg.Title = "Nexus API"
	}
	if cfg.Version == "" {
		cfg.Version = "1.0.0"
	}
	if cfg.SwaggerUIVersion == "" {
		cfg.SwaggerUIVersion = "5.20.1"
	}
}

// boolOr resolves a *bool with a fallback. Trivial helper that keeps
// the call sites readable.
func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

// publicRoot answers "should we also mount /openapi.json at the root?"
func (s *pluginState) publicRoot() bool { return boolOr(s.cfg.PublicRoot, true) }

// excludeAdmin answers "should we omit /__nexus/* paths from the spec?"
func (s *pluginState) excludeAdmin() bool { return boolOr(s.cfg.ExcludeAdmin, true) }

// includeGraphQL answers "should we include GraphQL ops in the spec?"
func (s *pluginState) includeGraphQL() bool { return boolOr(s.cfg.IncludeGraphQL, false) }

// _ avoids an unused-import error when gin is referenced only via
// handler signatures defined in other files of this package.
var _ gin.HandlerFunc = nil
