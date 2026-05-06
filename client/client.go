// Package client embeds and serves the nexus client SDK — a JS/TS
// runtime, generated TypeScript types, and Vue 3 composables —
// directly from the Go binary. No separate npm package; the SDK
// ships with the app.
//
// Mount installs four routes under cfg.Path (default
// "/__nexus/client"):
//
//	GET  <path>/manifest.json    application/json
//	GET  <path>/client.js        application/javascript (ESM)
//	GET  <path>/client.d.ts      application/typescript (generated)
//	GET  <path>/vue.js           application/javascript (ESM, Vue 3)
//
// The manifest is the foundation. It enumerates every endpoint
// (REST, GraphQL, WebSocket), their typed args/return schemas, the
// running auth strategy, and a deduped pool of named struct types.
// client.js fetches it on construct; client.d.ts is generated from
// it at boot.
package client

import (
	"embed"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/paulmanoni/nexus/registry"
)

// DefaultPath is the URL prefix the SDK routes mount under when
// Config.Path is empty. Sits beside the dashboard at /__nexus so
// the framework's "introspection + tooling surface" stays under
// one namespace.
const DefaultPath = "/__nexus/client"

// Config tunes the embedded SDK auto-mount. Zero value Enabled=false
// so omitting Config.Client from nexus.Config is safe — apps that
// don't ship an SDK don't pay the embed cost.
type Config struct {
	// Enabled gates the entire mount. Default false.
	Enabled bool

	// Path is the URL prefix the SDK routes mount under. Default
	// DefaultPath ("/__nexus/client"). The dashboard at /__nexus/*
	// and the SDK at /__nexus/client/* share an obvious namespace;
	// the SDK routes are PUBLIC (no admin-token gate) because a
	// browser bundle has to fetch them at runtime.
	Path string

	// Middleware applies to every SDK route. Useful when an app
	// gates its dashboard but wants the SDK manifest publicly
	// readable. Empty by default — the SDK is meant to be public.
	Middleware []gin.HandlerFunc

	// Manifest overrides the default per-build manifest projection.
	// Most apps leave this nil; the framework's default reads the
	// live registry. Override for multi-tenant filtering or custom
	// type-stripping. Called once on first request, then cached;
	// see Handler.Reload to invalidate.
	Manifest func() Manifest
}

// Handler holds the live state of a mounted SDK surface — the
// cached manifest + .d.ts strings and a sync.Once that gates the
// first build. Returned by Mount so tests (and apps that need to
// invalidate the cache) can call Reload().
type Handler struct {
	cfg        Config
	reg        *registry.Registry
	authInfo   func() ExtractorInfo
	schemaRefs func() map[string]registry.NamedType
	basePath   string

	mu       sync.Mutex
	once     *sync.Once
	manifest []byte
	dts      []byte
}

// Reload drops the cached manifest + .d.ts so the next request
// rebuilds. Used by tests and by apps that register endpoints via
// late mount paths. Production callers usually don't need it —
// the registry stops mutating after fx.Start, so the first build
// is the only build.
func (h *Handler) Reload() {
	h.mu.Lock()
	h.once = &sync.Once{}
	h.manifest = nil
	h.dts = nil
	h.mu.Unlock()
}

// SetAuthInfo installs (or replaces) the auth-info provider used to
// populate the manifest's Auth section on next build. Wired by
// auth.Module's option chain when both the SDK and auth are in
// the same app — separates the cycle (client/ doesn't import auth/,
// auth/ doesn't import client/, the bridge lives in nexus/).
//
// Calls Reload internally so the cached manifest rebuilds with the
// new auth shape on the next request.
func (h *Handler) SetAuthInfo(fn func() ExtractorInfo) {
	h.mu.Lock()
	h.authInfo = fn
	h.once = &sync.Once{}
	h.manifest = nil
	h.dts = nil
	h.mu.Unlock()
}

// Manifest returns the projected SDK manifest, building it from
// the registry on first call and caching the result. Exposed so
// tests can introspect the manifest without going through the HTTP
// layer.
func (h *Handler) Manifest() Manifest {
	if h.cfg.Manifest != nil {
		return h.cfg.Manifest()
	}
	var refs map[string]registry.NamedType
	if h.schemaRefs != nil {
		refs = h.schemaRefs()
	}
	return buildManifest(h.reg, h.authInfo, refs, h.basePath)
}

// build serializes the manifest + .d.ts once. sync.Once-protected;
// Reload swaps the Once so a subsequent call rebuilds. Both
// artifacts come from the same projected Manifest so they describe
// a coherent point-in-time view of the registry.
func (h *Handler) build() {
	h.mu.Lock()
	once := h.once
	h.mu.Unlock()
	once.Do(func() {
		m := h.Manifest()
		body, err := json.MarshalIndent(m, "", "  ")
		dts := GenerateDTS(m)
		h.mu.Lock()
		if err == nil {
			h.manifest = body
		}
		h.dts = []byte(dts)
		h.mu.Unlock()
	})
}

//go:embed ui/nexus-client.js
var clientJS []byte

//go:embed ui/nexus-vue.js
var vueJS []byte

// RuntimeJS returns the embedded nexus-client.js bytes. Public so
// the `nexus client --out` CLI can dump the runtime to disk
// without re-embedding a copy in cmd/nexus. Returns the same byte
// slice the HTTP handler serves at <path>/client.js.
func RuntimeJS() []byte { return clientJS }

// VueJS returns the embedded nexus-vue.js bytes. Same role as
// RuntimeJS for the Vue 3 composables module.
func VueJS() []byte { return vueJS }

// Mount installs the SDK routes on engine. Idempotent: re-mounting
// rebuilds the cached manifest by invalidating the Once. Called
// automatically by nexus.New when Config.Client.Enabled.
//
// authInfo may be nil — apps without auth.Module wired pass nil and
// the manifest's Auth section is omitted. When non-nil, the callback
// returns the active extractor strategy + parameters; nexus.New
// builds it from *auth.Manager.Info() and supplies it here.
//
// schemaRefs is the live pool of named-struct types walked at
// endpoint registration; the manifest's Refs section reads from it.
// basePath is the deployment-wide route prefix (app.routePrefix),
// stamped onto Manifest.BasePath so the SDK prepends it to every
// call.
func Mount(e *gin.Engine, reg *registry.Registry, authInfo func() ExtractorInfo, schemaRefs func() map[string]registry.NamedType, basePath string, cfg Config) *Handler {
	if cfg.Path == "" {
		cfg.Path = DefaultPath
	}
	h := &Handler{
		cfg:        cfg,
		reg:        reg,
		authInfo:   authInfo,
		schemaRefs: schemaRefs,
		basePath:   basePath,
		once:       &sync.Once{},
	}

	g := e.Group(cfg.Path)
	for _, mw := range cfg.Middleware {
		if mw != nil {
			g.Use(mw)
		}
	}
	g.GET("/manifest.json", func(c *gin.Context) {
		h.build()
		h.mu.Lock()
		body := h.manifest
		h.mu.Unlock()
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
	})
	g.GET("/client.js", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", clientJS)
	})
	g.GET("/client.d.ts", func(c *gin.Context) {
		h.build()
		h.mu.Lock()
		body := h.dts
		h.mu.Unlock()
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/typescript; charset=utf-8", body)
	})
	g.GET("/vue.js", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", vueJS)
	})
	return h
}

// embed import shim — keeps the embed package referenced even when
// build tags or future refactors hide its constructors.
var _ embed.FS