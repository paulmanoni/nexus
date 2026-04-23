// Package gql mounts a GraphQL schema (typically assembled by
// github.com/paulmanoni/nexus/graph) onto Gin and introspects its operations
// into the nexus registry. nexus does NOT own schema assembly — the caller
// keeps using go-graph (or graphql-go directly) and hands nexus the finished *graphql.Schema.
package gql

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/graphql-go/graphql"
	graph "github.com/paulmanoni/nexus/graph"

	"github.com/paulmanoni/nexus/ratelimit"
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
}

// Option is the variadic form of Options for builder-style callsites.
type Option func(*Options)

func WithUserDetailsFn(fn func(ctx context.Context, token string) (context.Context, any, error)) Option {
	return func(o *Options) { o.UserDetailsFn = fn }
}
func WithPlayground(v bool) Option { return func(o *Options) { o.Playground = v } }
func WithPretty(v bool) Option     { return func(o *Options) { o.Pretty = v } }
func WithDEBUG(v bool) Option      { return func(o *Options) { o.DEBUG = v } }

// Mount attaches schema at path for POST/GET and auto-registers every
// operation (queries, mutations, subscriptions) into the registry for the
// dashboard. If bus != nil, requests are traced.
//
// When any option touches auth (UserDetailsFn), playground, or debug, the
// adapter routes requests through graph.NewHTTP. Otherwise the default plain
// graphql.Do handler is used — keeping graphql-go-only users unaffected.
func Mount(e *gin.Engine, r *registry.Registry, bus *trace.Bus, service, path string, schema *graphql.Schema, opts ...Option) {
	var cfg Options
	for _, o := range opts {
		o(&cfg)
	}
	registerOps(r, service, path, schema)

	var h gin.HandlerFunc
	if cfg.UserDetailsFn != nil || cfg.Playground || cfg.DEBUG {
		h = goGraphHandler(schema, cfg)
	} else {
		h = simpleHandler(schema)
	}

	var handlers []gin.HandlerFunc
	if bus != nil {
		handlers = append(handlers, trace.Middleware(bus, service, "POST "+path, string(registry.GraphQL)))
	}
	// Stash the caller IP in the request context so per-op middleware
	// downstream (rate-limit, metrics error recorder) can attribute the
	// request without the gql adapter leaking gin.Context into graph.
	// Runs whether the underlying handler is the simple path or graph's
	// Playground-capable NewHTTP.
	handlers = append(handlers, func(c *gin.Context) {
		ctx := ratelimit.WithClientIP(c.Request.Context(), c.ClientIP())
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	handlers = append(handlers, h)
	e.POST(path, handlers...)
	e.GET(path, handlers...)
}

type request struct {
	Query         string         `json:"query"         form:"query"`
	OperationName string         `json:"operationName" form:"operationName"`
	Variables     map[string]any `json:"variables"`
}

func simpleHandler(schema *graphql.Schema) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req request
		if c.Request.Method == http.MethodGet {
			req.Query = c.Query("query")
			req.OperationName = c.Query("operationName")
			if v := c.Query("variables"); v != "" {
				_ = json.Unmarshal([]byte(v), &req.Variables)
			}
		} else {
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		// Caller IP is already stashed in the request context by a
		// route-level middleware in Mount, so any downstream reader
		// (rate-limit, metrics error recorder) sees it via
		// ratelimit.ClientIPFromCtx(ctx).
		result := graphql.Do(graphql.Params{
			Schema:         *schema,
			RequestString:  req.Query,
			VariableValues: req.Variables,
			OperationName:  req.OperationName,
			Context:        c.Request.Context(),
		})
		c.JSON(http.StatusOK, result)
	}
}

// goGraphHandler delegates to graph.NewHTTP so resolvers can read user
// details out of rootValue and the Playground works. nexus still owns
// tracing and middleware composition at the Gin layer.
func goGraphHandler(schema *graphql.Schema, cfg Options) gin.HandlerFunc {
	h := graph.NewHTTP(&graph.GraphContext{
		Schema:        schema,
		Playground:    cfg.Playground,
		Pretty:        cfg.Pretty,
		DEBUG:         cfg.DEBUG,
		UserDetailsFn: cfg.UserDetailsFn,
	})
	return gin.WrapF(h)
}

func registerOps(r *registry.Registry, service, path string, schema *graphql.Schema) {
	record := func(kind, name string, f *graphql.FieldDefinition) {
		r.RegisterEndpoint(registry.Endpoint{
			Service:     service,
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
