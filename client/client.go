// Package client embeds and serves the nexus client SDK — a JS/TS
// runtime, generated TypeScript types, and Vue 3 composables —
// directly from the Go binary. No separate npm package; the SDK
// ships with the app.
//
// Mount installs five routes under cfg.Path (default
// "/__nexus/client"):
//
//	GET  <path>/manifest.json    application/json
//	GET  <path>/client.js        application/javascript (ESM)
//	GET  <path>/client.d.ts      application/typescript (paired with client.js)
//	GET  <path>/vue.js           application/javascript (ESM, Vue 3)
//	GET  <path>/vue.d.ts         application/typescript (paired with vue.js)
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
	// the runtime files (client.js, vue.js, *.d.ts) are always
	// served unauthenticated since browsers fetch them anonymously
	// before any login. The MANIFEST is gated by the Public flag.
	Path string

	// Public, when true, exposes the FULL manifest (every endpoint,
	// every schema, every named ref, every WS path) on the
	// unauthenticated /manifest.json route — the v0.28.x default.
	//
	// Default false: the public manifest is stripped to the safe
	// minimum (Version, BasePath, Auth section, auth-flagged
	// endpoints only) so an anonymous browser can still complete
	// the login flow without leaking the API surface to scrapers.
	//
	// What works on each setting (runtime, browser-side):
	//
	//	             Public: false (default)   Public: true
	//	nx.auth.*    ✓                          ✓
	//	nx.rest      ✓ (path/args caller-side)  ✓
	//	nx.ws        ✓ (path caller-side)       ✓
	//	nx.query     ✗ (op lookup needs full)   ✓
	//	nx.mutate    ✗                          ✓
	//	nx.crud      ✗                          ✓
	//
	// TypeScript completion is unaffected by this flag — types
	// come from the dumped sdk/client.d.ts (vendored at build
	// time), not the runtime manifest.
	//
	// Recommended pattern for production: leave Public false on
	// public-facing deployments; flip to true only on internal
	// admin/dev listeners (compose with nexus.IfDeployment).
	Public bool

	// Middleware applies to every SDK route. Empty by default. For
	// the manifest specifically, prefer the Public flag — Middleware
	// gates the runtime .js files too, which most apps don't want.
	Middleware []gin.HandlerFunc

	// Manifest overrides the default per-build manifest projection.
	// Most apps leave this nil; the framework's default reads the
	// live registry. Override for multi-tenant filtering or custom
	// type-stripping. Called once on first request, then cached;
	// see Handler.Reload to invalidate.
	Manifest func() Manifest

	// OutDir, when non-empty, makes Mount also dump the SDK files
	// (client.js + client.d.ts, vue.js + vue.d.ts, manifest.json) to
	// disk on startup so a frontend's filesystem-based tooling
	// (TypeScript compiler, Vite, JetBrains, VS Code) can resolve
	// types and runtime imports without a manual `nexus client --out`
	// step. Each .js sits next to its .d.ts so TS auto-pairs them
	// regardless of whether the import uses the runtime URL or a
	// plain relative path.
	//
	// Idempotent: files are byte-compared and skipped when content
	// matches, preserving mtime — file-watcher reloads + IDE
	// re-indexing don't fire on no-op restarts.
	//
	// Recommend leaving empty in production builds. Apps that need
	// it gated to dev only can branch on an env var when building
	// the Config (this field intentionally has no built-in env
	// magic — explicit beats implicit for "writes files to your
	// project tree").
	OutDir string

	// TSConfig, when non-empty, makes Mount merge path mappings
	// (the runtime URL imports → OutDir files) into the named
	// jsconfig.json or tsconfig.json on startup. Existing fields
	// (compilerOptions.target, include, exclude, custom paths)
	// survive untouched. Same idempotent contract as OutDir.
	//
	// Only takes effect when OutDir is also set — the path
	// mappings need a target.
	TSConfig string

	// ViteConfig, when non-empty, makes Mount auto-attach the
	// nexus-vite-plugin (auto-select) to a Vite config on startup.
	// Two idempotent edits land in the file:
	//
	//   1. an `import nexusAutoSelect from '<rel>/nexus-vite-plugin.js'`
	//      after the last top-level import,
	//   2. a `nexusAutoSelect()` entry inside the first `plugins:`
	//      array.
	//
	// Path can be absolute or relative to the Go binary's CWD.
	// Only takes effect when OutDir is set — the plugin file lives
	// under OutDir. Re-running is a no-op once both edits have
	// landed; the framework keys idempotence off the literal
	// "nexusAutoSelect" identifier in the file.
	//
	// Recommend gating this behind a dev-mode flag in production
	// builds — there's no reason to mutate a checked-in config on
	// every prod boot.
	ViteConfig string
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

	mu        sync.Mutex
	once      *sync.Once
	manifest  []byte
	dtsClient []byte
	dtsVue    []byte
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
	h.dtsClient = nil
	h.dtsVue = nil
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
	h.dtsClient = nil
	h.dtsVue = nil
	h.mu.Unlock()
}

// Manifest returns the projected SDK manifest, building it from
// the registry on first call and caching the result. Exposed so
// tests can introspect the manifest without going through the HTTP
// layer.
//
// When cfg.Public is false (the default), the result is the
// FULL manifest — the same shape used internally for code
// generation and for the public route when Public is true.
// Tests + the .d.ts generator always see the full shape so type
// output is identical regardless of the runtime gating.
//
// publicManifest() is the projection used for the unauthenticated
// HTTP route only.
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

// publicManifest returns the manifest shape served to anonymous
// browsers at GET <path>/manifest.json. With cfg.Public the full
// manifest goes out; without it, the result is stripped to the
// minimum needed for the runtime's auth flows + plain nx.rest()
// calls (no schemas, no ref pool, no GraphQL/CRUD/WS endpoints,
// no service list).
//
// The stripping happens at HTTP-projection time, not at build
// time — the .d.ts always reflects the full schema since types
// are vendored at the consumer's tsc compile step, not fetched at
// runtime.
func (h *Handler) publicManifest() Manifest {
	full := h.Manifest()
	if h.cfg.Public {
		return full
	}
	skinny := Manifest{
		Version:  full.Version,
		BasePath: full.BasePath,
		Auth:     full.Auth,
	}
	// Keep auth-flagged endpoints (login/logout/me) so the SDK's
	// auth namespace can resolve their paths post-construct.
	// Strip Args + RequiresPerm in all cases — neither feeds the
	// runtime's auth dispatch. Return stays ONLY for GraphQL auth
	// ops because _gql's selection-set hint reads it (object
	// returns need "{ __typename }"). REST auth ops still have
	// Return stripped, preserving the original "no schemas in the
	// public projection" contract for the non-graphql path.
	for _, e := range full.Endpoints {
		if e.AuthFlow == "" {
			continue
		}
		stripped := e
		stripped.Args = nil
		stripped.RequiresPerm = nil
		if e.Transport != string(registry.GraphQL) {
			stripped.Return = nil
		}
		skinny.Endpoints = append(skinny.Endpoints, stripped)
	}
	// GraphQL auth ops need their Return type's refs resolvable on the
	// browser side — _gql's selection-set walker reads manifest.refs to
	// expand object returns into concrete fields. Without this, login
	// responses come back as `{__typename: "..."}` with no data. We
	// project only the refs reachable from the preserved auth-flow
	// endpoints, keeping the public surface minimal.
	skinny.Refs = collectAuthFlowRefs(skinny.Endpoints, full.Refs)
	return skinny
}

// collectAuthFlowRefs walks the Return TypeRefs of every endpoint in
// `eps` (typically the auth-flow projection) and returns a Refs
// subset of `pool` containing every named type transitively reachable.
// Returns nil when no refs are needed so the JSON projection stays
// `refs` omittable.
func collectAuthFlowRefs(eps []EndpointInfo, pool map[string]registry.NamedType) map[string]registry.NamedType {
	if len(pool) == 0 {
		return nil
	}
	needed := map[string]struct{}{}
	for _, e := range eps {
		collectRefNames(e.Return, needed)
	}
	if len(needed) == 0 {
		return nil
	}
	out := make(map[string]registry.NamedType, len(needed))
	// Transitive closure — refs may reference other refs through
	// nested fields. Iterate until the needed set stops growing.
	for changed := true; changed; {
		changed = false
		for name := range needed {
			if _, done := out[name]; done {
				continue
			}
			nt, ok := pool[name]
			if !ok {
				// Mark resolved-but-missing so we don't loop on it.
				out[name] = registry.NamedType{}
				delete(out, name)
				continue
			}
			out[name] = nt
			for _, f := range nt.Fields {
				collectRefNames(&f.Type, needed)
			}
			changed = true
		}
	}
	return out
}

func collectRefNames(t *registry.TypeRef, into map[string]struct{}) {
	if t == nil {
		return
	}
	if t.Kind == "ref" && t.Ref != "" {
		into[t.Ref] = struct{}{}
	}
	if t.Of != nil {
		collectRefNames(t.Of, into)
	}
	if t.KeyOf != nil {
		collectRefNames(t.KeyOf, into)
	}
	if t.Object != nil {
		for _, f := range t.Object.Fields {
			collectRefNames(&f.Type, into)
		}
	}
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
		// The public projection is what the unauthenticated HTTP
		// route serves; the full one feeds .d.ts generation +
		// h.Manifest() callers (tests, dump, in-process consumers)
		// so types stay complete regardless of the runtime gating.
		pub := h.publicManifest()
		body, err := json.MarshalIndent(pub, "", "  ")
		clientDTS := GenerateClientDTS(m)
		vueDTS := GenerateVueDTS(m)
		h.mu.Lock()
		if err == nil {
			h.manifest = body
		}
		h.dtsClient = []byte(clientDTS)
		h.dtsVue = []byte(vueDTS)
		h.mu.Unlock()
	})
}

//go:embed ui/nexus-client.js
var clientJS []byte

//go:embed ui/nexus-vue.js
var vueJS []byte

//go:embed ui/nexus-vite-plugin.js
var vitePluginJS []byte

// RuntimeJS returns the embedded nexus-client.js bytes. Public so
// the `nexus client --out` CLI can dump the runtime to disk
// without re-embedding a copy in cmd/nexus. Returns the same byte
// slice the HTTP handler serves at <path>/client.js.
func RuntimeJS() []byte { return clientJS }

// VueJS returns the embedded nexus-vue.js bytes. Same role as
// RuntimeJS for the Vue 3 composables module.
func VueJS() []byte { return vueJS }

// VitePluginJS returns the embedded nexus-vite-plugin.js bytes —
// the build-time auto-select transformer that rewrites nx.query /
// nx.mutate calls to fetch only the fields the consumer reads.
// Dumped into OutDir alongside client.js / vue.js when OutDir is
// configured; users wire it into vite.config.ts manually.
func VitePluginJS() []byte { return vitePluginJS }

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
//
// When cfg.OutDir is set, Mount also dumps the SDK files to disk
// after registering routes so frontend tooling (TS compiler, IDE)
// can resolve types/imports without a manual `nexus client --out`
// step. Dump errors are logged but don't fail Mount — dev-tool
// convenience shouldn't crash boot.
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
		body := h.dtsClient
		h.mu.Unlock()
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/typescript; charset=utf-8", body)
	})
	g.GET("/vue.js", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", vueJS)
	})
	g.GET("/vue.d.ts", func(c *gin.Context) {
		h.build()
		h.mu.Lock()
		body := h.dtsVue
		h.mu.Unlock()
		c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
		c.Data(http.StatusOK, "application/typescript; charset=utf-8", body)
	})
	// Auto-dump on cfg.OutDir is wired by the caller via a
	// fx.Lifecycle.OnStart hook (see nexus.New). Mount can't dump
	// here because it runs inside the *App constructor —
	// AsRest/AsQuery invokes haven't yet populated the registry,
	// so the .d.ts would be empty.
	return h
}

// embed import shim — keeps the embed package referenced even when
// build tags or future refactors hide its constructors.
var _ embed.FS