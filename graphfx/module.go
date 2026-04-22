// Package graphfx is the glue between nexus and github.com/paulmanoni/go-graph.
// Drop graphfx.Module into your fx.New(...), wrap your resolver constructors
// with AsQuery/AsMutation/AsSubscription, and mount with ServeAt.
//
// As of go-graph v1.2.4, graphfx introspects every resolver after mounting and
// enriches the nexus registry with: return type, per-arg Required flag and
// default values, per-arg validator metadata (from graph.WithArgValidator),
// per-resolver named middleware chain (from graph.WithNamedMiddleware), and
// deprecation info (from graph.WithDeprecated). Dashboards consume this without
// the user declaring anything separately.
//
// Typical usage:
//
//	fx.New(
//	    fxmod.Module,
//	    graphfx.Module,
//	    fx.Provide(
//	        graphfx.AsQuery(NewGetAllAdverts),
//	        graphfx.AsMutation(NewCreateAdvert),
//	    ),
//	    graphfx.ServeAt("adverts", "/graphql",
//	        graphfx.Describe("Job adverts catalog"),
//	        graphfx.UseDefaults(),
//	    ),
//	)
package graphfx

import (
	graph "github.com/paulmanoni/go-graph"
	"go.uber.org/fx"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/transport/gql"
)

// Module provides *Bundle into the fx graph. A Bundle holds both the assembled
// *graph.SchemaBuilder and the raw resolver slices, so ServeAt can introspect
// every resolver after mount.
var Module = fx.Module("graphfx",
	fx.Provide(provideBundle),
)

// AsQuery annotates a constructor so Fx stores its result in the
// "query_fields" value group that SchemaBuilderParams consumes.
func AsQuery(ctor any) any {
	return fx.Annotate(ctor, fx.ResultTags(`group:"query_fields"`))
}

// AsMutation — like AsQuery, for mutations.
func AsMutation(ctor any) any {
	return fx.Annotate(ctor, fx.ResultTags(`group:"mutation_fields"`))
}

// AsSubscription — like AsQuery, for subscriptions.
func AsSubscription(ctor any) any {
	return fx.Annotate(ctor, fx.ResultTags(`group:"subscription_fields"`))
}

// Bundle carries both the schema builder and the raw field slices so ServeAt
// can introspect each resolver after mount. You normally don't construct this
// yourself; graphfx.Module provides it.
type Bundle struct {
	Builder       *graph.SchemaBuilder
	Queries       []graph.QueryField
	Mutations     []graph.MutationField
	Subscriptions []graph.SubscriptionField
}

type schemaBuilderIn struct {
	fx.In
	QueryFields        []graph.QueryField        `group:"query_fields"`
	MutationFields     []graph.MutationField     `group:"mutation_fields"`
	SubscriptionFields []graph.SubscriptionField `group:"subscription_fields"`
}

func provideBundle(in schemaBuilderIn) *Bundle {
	return &Bundle{
		Builder: graph.NewSchemaBuilder(graph.SchemaBuilderParams{
			QueryFields:        in.QueryFields,
			MutationFields:     in.MutationFields,
			SubscriptionFields: in.SubscriptionFields,
		}),
		Queries:       in.QueryFields,
		Mutations:     in.MutationFields,
		Subscriptions: in.SubscriptionFields,
	}
}

// --- ServeAt + opts ----------------------------------------------------------

type mountConfig struct {
	description string
	useNames    []string
	useDefaults bool
	gqlOpts     []gql.Option
}

// Opt configures a ServeAt call.
type Opt func(*mountConfig)

// Describe sets the service's human description (dashboard header).
func Describe(s string) Opt { return func(c *mountConfig) { c.description = s } }

// Use attaches the named resources to the service. Empty string = default DB.
func Use(names ...string) Opt {
	return func(c *mountConfig) { c.useNames = append(c.useNames, names...) }
}

// UseDefaults attaches the default database, cache, and queue (if registered).
func UseDefaults() Opt { return func(c *mountConfig) { c.useDefaults = true } }

// WithGQL forwards gql.Option values (UserDetailsFn, Playground, Pretty,
// DEBUG) to the underlying MountGraphQL call. Typical use:
//
//	graphfx.ServeAt("adverts", "/graphql",
//	    graphfx.WithGQL(gql.WithUserDetailsFn(auth.Fetch)),
//	)
func WithGQL(opts ...gql.Option) Opt {
	return func(c *mountConfig) { c.gqlOpts = append(c.gqlOpts, opts...) }
}

// ServeAt returns an fx.Option that, once fx.Start runs:
//  1. builds the schema from the injected *Bundle
//  2. mounts it on the named service at path
//  3. introspects every resolver and patches the registry with per-resolver
//     middleware names, arg validators, return types, and deprecation
func ServeAt(service, path string, opts ...Opt) fx.Option {
	return fx.Invoke(func(app *nexus.App, b *Bundle) {
		cfg := &mountConfig{}
		for _, opt := range opts {
			opt(cfg)
		}
		schema, err := b.Builder.Build()
		if err != nil {
			panic("graphfx: schema build failed: " + err.Error())
		}
		svc := app.Service(service)
		if cfg.description != "" {
			svc.Describe(cfg.description)
		}
		if cfg.useDefaults {
			svc.UsingDefaults()
		}
		if len(cfg.useNames) > 0 {
			svc.Using(cfg.useNames...)
		}
		svc.MountGraphQL(path, &schema, cfg.gqlOpts...)

		enrichFromIntrospection(app.Registry(), service, b)
	})
}

// Enrich walks every query/mutation/subscription field in the Bundle and
// patches the nexus registry with per-resolver introspection data (return
// type, arg validators, middleware names, deprecation). Use this when you
// mount the schema yourself — e.g. you need a UserDetailsFn that depends on
// an Fx-provided service and so can't go through graphfx.ServeAt's options.
//
//	fx.Invoke(func(app *nexus.App, b *graphfx.Bundle, u *pkg.UserMiddleware) {
//	    schema, _ := b.Builder.Build()
//	    app.Service("graph").MountGraphQL("/graphql", &schema,
//	        gql.WithUserDetailsFn(u.Fetch))
//	    graphfx.Enrich(app.Registry(), "graph", b)
//	})
func Enrich(reg *registry.Registry, service string, b *Bundle) {
	enrichFromIntrospection(reg, service, b)
}

// enrichFromIntrospection walks every query/mutation field, pulls its
// FieldInfo via graph.Inspect, and patches the corresponding nexus registry
// endpoint. Any middleware name it sees is also registered so the /middlewares
// endpoint lists custom-but-declared middlewares.
func enrichFromIntrospection(reg *registry.Registry, service string, b *Bundle) {
	for _, q := range b.Queries {
		patch(reg, service, q)
	}
	for _, m := range b.Mutations {
		patch(reg, service, m)
	}
	for _, s := range b.Subscriptions {
		patch(reg, service, s)
	}
}

func patch(reg *registry.Registry, service string, f any) {
	info, ok := graph.Inspect(f)
	if !ok {
		return
	}
	for _, mw := range info.Middlewares {
		if mw.Name != "" && mw.Name != "anonymous" {
			reg.EnsureMiddleware(mw.Name)
		}
	}
	update := registry.GraphQLUpdate{
		Description:       info.Description,
		ReturnType:        info.ReturnType,
		Args:              convertArgs(info.Args),
		Middleware:        middlewareNames(info.Middlewares),
		Deprecated:        info.Deprecated,
		DeprecationReason: info.DeprecationReason,
	}
	reg.UpdateGraphQLEndpoint(service, info.Name, update)
}

func middlewareNames(ms []graph.MiddlewareInfo) []string {
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		name := m.Name
		if name == "" {
			name = "anonymous"
		}
		out = append(out, name)
	}
	return out
}

func convertArgs(args []graph.ArgInfo) []registry.GraphQLArg {
	if len(args) == 0 {
		return nil
	}
	out := make([]registry.GraphQLArg, 0, len(args))
	for _, a := range args {
		validators := make([]registry.GraphQLValidator, 0, len(a.Validators))
		for _, v := range a.Validators {
			validators = append(validators, registry.GraphQLValidator{
				Kind:    v.Kind,
				Message: v.Message,
				Details: v.Details,
			})
		}
		out = append(out, registry.GraphQLArg{
			Name:        a.Name,
			Type:        a.Type,
			Description: a.Description,
			Required:    a.Required,
			Default:     a.DefaultValue,
			Validators:  validators,
		})
	}
	return out
}
