// Package gql mounts a GraphQL schema (typically assembled by
// github.com/paulmanoni/nexus/graph) onto Gin and introspects its operations
// into the nexus registry. nexus does NOT own schema assembly — the caller
// keeps using go-graph (or graphql-go directly) and hands nexus the finished *graphql.Schema.
package gql

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
	"github.com/paulmanoni/nexus/dataloader"
	graph "github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/httpx"
	"github.com/paulmanoni/nexus/internal/maskhook"

	"github.com/paulmanoni/nexus/extension/ratelimit"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/trace"
)

// Options tunes how the GraphQL endpoint is served. Pass values via the
// WithXxx Option funcs.
type Options struct {
	// UserDetailsFn, when set, routes requests through graph.NewHTTP so
	// resolvers can call graph.GetRootInfo(p, "details", &user). Without it
	// the adapter uses plain graphql.Do with no user injection.
	UserDetailsFn func(ctx context.Context, token string) (context.Context, any, error)

	// Playground enables go-graph's Playground UI at the mount path on GET.
	Playground bool

	// Pretty-prints JSON responses.
	Pretty bool

	// DEBUG disables validation/sanitization in go-graph. Use only in dev.
	DEBUG bool

	// ServiceForField overrides the registered service per GraphQL field
	// name. When N service partitions share one /graphql path (autoMount
	// merges their schemas to avoid double-registering routes), each
	// field needs to record under its OWN service in the registry — not
	// the path's owner service. autoMountGraphQL builds this lookup
	// from its servicePartition list and threads it through Mount; the
	// dashboard then groups endpoints by the right service. Nil falls
	// back to the mount-level `service` arg.
	ServiceForField func(fieldName string) string

	// AllowIntrospection, when non-nil, is consulted on every request
	// before the resolver runs. Returning false on a request whose
	// query contains __schema / __type makes the handler 404 — the
	// schema-shape leak the public-prod safety pass is closing. Nil
	// means introspection is allowed unconditionally (the dev/internal
	// default; nexus.New supplies a closure that reads
	// Config.Introspection + IntrospectionNetworks).
	AllowIntrospection func(c *httpx.Ctx) bool

	// DocumentCache memoizes parse + validate for repeat queries.
	// Profiling shows ~89% of a GraphQL request's allocations come
	// from those two phases; caching the parsed AST drops the
	// per-request cost dramatically for the typical "same query
	// repeated with different variables" pattern. Nil disables.
	// nexus.autoMount wires a default cache via WithDocumentCache
	// (size from Config.GraphQL.DocumentCacheSize).
	DocumentCache *DocumentCache

	// StatsRegistry, when non-nil along with DocumentCache, is used
	// to register this mount's cache so the dashboard can surface
	// hit/miss/eviction counters. nexus.autoMount wires this from
	// (*App).gqlStats.
	StatsRegistry *StatsRegistry
}

// Option is the variadic form of Options for builder-style callsites.
type Option func(*Options)

func WithUserDetailsFn(fn func(ctx context.Context, token string) (context.Context, any, error)) Option {
	return func(o *Options) { o.UserDetailsFn = fn }
}
func WithPlayground(v bool) Option { return func(o *Options) { o.Playground = v } }
func WithPretty(v bool) Option     { return func(o *Options) { o.Pretty = v } }
func WithDEBUG(v bool) Option      { return func(o *Options) { o.DEBUG = v } }

// WithServiceForField installs a per-field service-name resolver.
// See Options.ServiceForField.
func WithServiceForField(fn func(name string) string) Option {
	return func(o *Options) { o.ServiceForField = fn }
}

// WithAllowIntrospection installs a per-request gate. When fn returns
// false, queries containing __schema / __type tokens 404 instead of
// resolving. Used by nexus.New to wire Config.Introspection +
// IntrospectionNetworks through to the GraphQL handler so the gate is
// consistent with the dashboard.
func WithAllowIntrospection(fn func(c *httpx.Ctx) bool) Option {
	return func(o *Options) { o.AllowIntrospection = fn }
}

// WithDocumentCache installs an LRU memo over parse + validate.
// capacity <= 0 disables. nexus.autoMount calls this with the
// value of Config.GraphQL.DocumentCacheSize (default 1024); pass
// WithDocumentCache(0) at the service level to turn it off.
func WithDocumentCache(capacity int) Option {
	return func(o *Options) {
		o.DocumentCache = NewDocumentCache(capacity)
	}
}

// WithStatsRegistry hands Mount the per-app cache registry the
// dashboard reads from. Each Mount call records its DocumentCache
// under the mount path so /__nexus/graphql/cache and the live WS
// snapshot can surface per-mount counters. nexus.autoMount calls
// this with (*App).gqlStats; raw graphql-go users can pass their
// own NewStatsRegistry() if they want the dashboard hook.
func WithStatsRegistry(r *StatsRegistry) Option {
	return func(o *Options) { o.StatsRegistry = r }
}

// Mount attaches schema at path for POST/GET and auto-registers every
// operation (queries, mutations, subscriptions) into the registry for the
// dashboard. If bus != nil, requests are traced.
//
// When any option touches auth (UserDetailsFn), playground, or debug, the
// adapter routes requests through graph.NewHTTP. Otherwise the default plain
// graphql.Do handler is used — keeping graphql-go-only users unaffected.
func Mount(e httpx.Router, r *registry.Registry, bus *trace.Bus, service, path string, schema *graphql.Schema, opts ...Option) {
	var cfg Options
	for _, o := range opts {
		o(&cfg)
	}
	registerOps(r, service, path, schema, cfg.ServiceForField)
	// Enroll this mount's cache so the dashboard can surface its
	// counters. Registry tolerates nil cache — no-op when caching
	// is disabled for this mount.
	if cfg.StatsRegistry != nil {
		cfg.StatsRegistry.Register(path, cfg.DocumentCache)
	}

	// POST is the hot path — JSON in, JSON out. cachedHandler runs
	// the cached-AST executor (skips parse+validate on a hit) and
	// builds the same rootValue / user-details map that
	// graph.NewHTTP would. GET keeps going through goGraphHandler
	// when Playground is on, since that path also serves the HTML
	// UI for browser visits.
	postHandler := cachedHandler(schema, cfg)
	var getHandler httpx.HandlerFunc
	if cfg.UserDetailsFn != nil || cfg.Playground || cfg.DEBUG {
		getHandler = goGraphHandler(schema, cfg)
	} else {
		getHandler = simpleHandler(schema)
	}
	// When the Playground is enabled, browser visits to the GraphQL path
	// get Apollo Sandbox as the default IDE instead of go-graph's bundled
	// Playground. GET queries from tooling (?query=) and Sandbox's own
	// POST traffic still flow through the inner handler untouched.
	if cfg.Playground {
		inner := getHandler
		sandbox := apolloSandboxHandler()
		getHandler = func(c *httpx.Ctx) {
			if isBrowserIDEVisit(c) {
				sandbox(c)
				return
			}
			inner(c)
		}
	}

	build := func(h httpx.HandlerFunc) []httpx.HandlerFunc {
		var hs []httpx.HandlerFunc
		if bus != nil {
			hs = append(hs, trace.Middleware(bus, service, "POST "+path, string(registry.GraphQL)))
		}
		// Production gate sits before the trace middleware unwrap so
		// blocked requests don't allocate a trace record. allow == nil
		// makes the gate a pass-through (no-op). When allow returns
		// false, the FULL go-graph security suite runs (depth, aliases,
		// complexity, no introspection) — matching what go-graph's
		// DEBUG: false / EnableValidation: true mode applies. When
		// allow returns true, validation is skipped (dev / admin /
		// allowlisted peer keeps the loose experience).
		if cfg.AllowIntrospection != nil {
			hs = append([]httpx.HandlerFunc{productionGate(cfg.AllowIntrospection, schema)}, hs...)
		}
		// Stash the caller IP in the request context so per-op middleware
		// downstream (rate-limit, metrics error recorder) can attribute the
		// request without the gql adapter leaking gin.Context into graph.
		hs = append(hs, func(c *httpx.Ctx) {
			ctx := ratelimit.WithClientIP(c.Request.Context(), c.ClientIP())
			c.Request = c.Request.WithContext(ctx)
			c.Next()
		})
		hs = append(hs, h)
		return hs
	}
	e.POST(path, build(postHandler)...)
	e.GET(path, build(getHandler)...)
}

type request struct {
	Query         string         `json:"query"         form:"query"`
	OperationName string         `json:"operationName" form:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func simpleHandler(schema *graphql.Schema) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		var req request
		if c.Request.Method == http.MethodGet {
			req.Query = c.Query("query")
			req.OperationName = c.Query("operationName")
			if v := c.Query("variables"); v != "" {
				// The POST path gets this from ShouldBindJSON; a GET
				// query string bypasses binding, so convert any masked
				// IDs here too.
				_ = json.Unmarshal(maskhook.UnmaskJSON([]byte(v)), &req.Variables)
			}
		} else {
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, httpx.H{"error": err.Error()})
				return
			}
		}
		// Install the status holder before graphql.Do so field
		// middlewares can call SetStatusCode(ctx, ...) to override
		// the default 200. Caller IP is already stashed by the
		// route-level middleware in Mount.
		ctx, _ := withStatusHolder(c.Request.Context())
		// Per-request dataloader registry — same wiring as the
		// cached fast path so resolvers behave identically here.
		ctx = dataloader.WithRegistry(ctx, dataloader.NewRegistry())
		result := graphql.Do(graphql.Params{
			Schema:         *schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        ctx,
		})
		status := http.StatusOK
		if s := statusFromCtx(ctx); s > 0 {
			status = s
		}
		c.JSON(status, result)
	}
}

// cachedHandler is the POST hot path. It parses the JSON body once,
// runs the cached executor (skipping parse+validate on a hit), and
// applies the same UserDetailsFn flow that graph.NewHTTP would —
// without going through github.com/graphql-go/handler. Falls back
// to the legacy goGraphHandler / simpleHandler path for unusual
// content types (form-encoded GraphQL queries are rare in
// practice; rather than reimplement that branch we delegate).
func cachedHandler(schema *graphql.Schema, cfg Options) httpx.HandlerFunc {
	fallback := simpleHandler(schema)
	if cfg.UserDetailsFn != nil || cfg.Playground || cfg.DEBUG {
		fallback = goGraphHandler(schema, cfg)
	}
	cache := cfg.DocumentCache
	return func(c *httpx.Ctx) {
		// Only JSON POSTs go through the cached fast path. Anything
		// else (form-encoded, multipart, etc.) falls back to the
		// existing handler so we don't have to re-implement those
		// edge cases here.
		ct := c.GetHeader("Content-Type")
		if !isJSONContentType(ct) {
			fallback(c)
			return
		}
		var req request
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, httpx.H{"error": err.Error()})
			return
		}
		// Install the status holder before resolvers run so field
		// middlewares can call SetStatusCode(ctx, ...). Mirrors
		// simpleHandler / goGraphHandler.
		ctx, _ := withStatusHolder(c.Request.Context())
		// Per-request dataloader registry. Lets resolvers call
		// dataloader.Get(ctx, name, fetch) to share one Loader
		// across siblings — the framework's N+1 escape hatch.
		// Allocated unconditionally because it's tiny (one map);
		// resolvers that don't use it pay the alloc and nothing
		// more.
		ctx = dataloader.WithRegistry(ctx, dataloader.NewRegistry())

		// User-details injection. Same shape as graph.NewHTTP's
		// RootObjectFn: token under "token", details under
		// "details". Only allocated when UserDetailsFn is set.
		var rootObj map[string]any
		if cfg.UserDetailsFn != nil {
			token := graph.ExtractBearerToken(c.Request)
			if token != "" {
				rootObj = map[string]any{"token": token}
				if newCtx, details, err := cfg.UserDetailsFn(ctx, token); err == nil {
					ctx = newCtx
					if details != nil {
						rootObj["details"] = details
					}
				}
			}
		}

		result := executeCached(cache, graphql.Params{
			Schema:         *schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        ctx,
			RootObject:     rootObj,
		})

		status := http.StatusOK
		if s := statusFromCtx(ctx); s > 0 {
			status = s
		}
		c.JSON(status, result)
	}
}

// executeCached is graphql.Do with parse + validate memoized through
// cache. When cache is nil it degenerates to graphql.Do exactly — so
// callers don't need to branch.
func executeCached(cache *DocumentCache, params graphql.Params) *graphql.Result {
	if cache == nil {
		return graphql.Do(params)
	}
	entry, hit := cache.Get(params.RequestString)
	if !hit {
		src := source.NewSource(&source.Source{
			Body: []byte(params.RequestString),
			Name: "GraphQL request",
		})
		doc, parseErr := parser.Parse(parser.ParseParams{Source: src})
		if parseErr != nil {
			// Cache parse failures too — the same query string will
			// always fail the same way, and the formatted errors are
			// already allocated.
			entry = &documentEntry{
				doc:     nil,
				valErrs: gqlerrors.FormatErrors(parseErr),
				valid:   false,
			}
			cache.Put(params.RequestString, entry)
			return &graphql.Result{Errors: entry.valErrs}
		}
		validation := graphql.ValidateDocument(&params.Schema, doc, nil)
		entry = &documentEntry{
			doc:     doc,
			valErrs: validation.Errors,
			valid:   validation.IsValid,
		}
		cache.Put(params.RequestString, entry)
	}
	if !entry.valid {
		// Validation failed previously — return the same errors
		// without re-running Execute. Matches graphql.Do's behavior
		// for invalid documents.
		return &graphql.Result{Errors: entry.valErrs}
	}
	return graphql.Execute(graphql.ExecuteParams{
		Schema:        params.Schema,
		Root:          params.RootObject,
		AST:           entry.doc,
		OperationName: params.OperationName,
		Args:          params.VariableValues,
		Context:       params.Context,
	})
}

// isJSONContentType returns true when ct names application/json (with
// optional parameters like ; charset=utf-8). Avoids pulling in mime
// for a one-shot prefix check.
func isJSONContentType(ct string) bool {
	if ct == "" {
		// graphql-go's handler treats missing Content-Type on POST
		// as JSON when the body parses; we match that for the
		// common case of curl without an explicit header.
		return true
	}
	// Find the first ';' or end-of-string and compare the prefix.
	end := len(ct)
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			end = i
			break
		}
	}
	// Trim trailing whitespace cheaply.
	for end > 0 && (ct[end-1] == ' ' || ct[end-1] == '\t') {
		end--
	}
	return ct[:end] == "application/json"
}

// goGraphHandler delegates to graph.NewHTTP so resolvers can read user
// details out of rootValue and the Playground works. nexus still owns
// tracing and middleware composition at the Gin layer.
//
// Status-code override: graph.NewHTTP writes the response directly
// (no hook between "compute result" and "send headers"), so the
// handler buffers the inner response via statusCaptureWriter, then
// replays it onto c.Writer with any override applied.
func goGraphHandler(schema *graphql.Schema, cfg Options) httpx.HandlerFunc {
	h := graph.NewHTTP(&graph.GraphContext{
		Schema:        schema,
		Playground:    cfg.Playground,
		Pretty:        cfg.Pretty,
		DEBUG:         cfg.DEBUG,
		UserDetailsFn: cfg.UserDetailsFn,
	})
	return func(c *httpx.Ctx) {
		ctx, _ := withStatusHolder(c.Request.Context())
		ctx = dataloader.WithRegistry(ctx, dataloader.NewRegistry())
		req := c.Request.WithContext(ctx)
		wrap := newStatusCaptureWriter(c.Writer)
		h(wrap, req)
		wrap.flush(statusFromCtx(ctx))
	}
}

func registerOps(r *registry.Registry, service, path string, schema *graphql.Schema, serviceForField func(string) string) {
	record := func(kind, name string, f *graphql.FieldDefinition) {
		svc := service
		if serviceForField != nil {
			if s := serviceForField(name); s != "" {
				svc = s
			}
		}
		r.RegisterEndpoint(registry.Endpoint{
			Service:     svc,
			Name:        name,
			Transport:   registry.GraphQL,
			Method:      kind,
			Path:        path,
			Description: f.Description,
			Args:        extractArgs(f.Args),
			ReturnType:  typeString(f.Type),
		})
	}
	if q := schema.QueryType(); q != nil {
		for name, f := range q.Fields() {
			record("query", name, f)
		}
	}
	if m := schema.MutationType(); m != nil {
		for name, f := range m.Fields() {
			record("mutation", name, f)
		}
	}
	if s := schema.SubscriptionType(); s != nil {
		for name, f := range s.Fields() {
			record("subscription", name, f)
		}
	}
}

func extractArgs(args []*graphql.Argument) []registry.GraphQLArg {
	if len(args) == 0 {
		return nil
	}
	out := make([]registry.GraphQLArg, 0, len(args))
	for _, a := range args {
		out = append(out, registry.GraphQLArg{
			Name:        a.Name(),
			Type:        typeString(a.Type),
			Description: a.Description(),
		})
	}
	return out
}

// typeString renders a graphql-go type as an SDL-like string ("String!", "[Int]").
// graphql-go's types implement fmt.Stringer for this purpose.
func typeString(t graphql.Type) string {
	if t == nil {
		return ""
	}
	return t.String()
}
