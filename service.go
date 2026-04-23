package nexus

import (
	"context"

	"github.com/graphql-go/graphql"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/transport/gql"
	"github.com/paulmanoni/nexus/transport/rest"
	"github.com/paulmanoni/nexus/transport/ws"
)

// Service is a named group of endpoints. Services are the nodes the dashboard
// draws in the architecture view.
//
// A Service also carries the GraphQL mount path for reflective controllers
// registered via nexus.AsQuery / AsMutation. The auto-mount step inside
// fxmod.Module reads this path at fx.Start and wires the schema onto the
// engine. Default is "/graphql"; override via (*Service).AtGraphQL.
type Service struct {
	app         *App
	name        string
	graphqlPath string

	// Per-service GraphQL knob. Playground / Debug / Pretty live on
	// nexus.Config because they're environment-level; Auth is per-service
	// because different services often differ (admin vs public).
	graphqlUserDetFn UserDetailsFn
}

// UserDetailsFn, when set on a service, routes GraphQL requests through
// graph.NewHTTP so resolvers can read the authenticated user via
// graph.GetRootInfo(p, "details", &user). Returning an error aborts the
// request with the framework's standard unauthenticated shape.
type UserDetailsFn func(ctx context.Context, token string) (context.Context, any, error)

// DefaultGraphQLPath is the mount path nexus.AsQuery / AsMutation use when a
// service hasn't called AtGraphQL.
const DefaultGraphQLPath = "/graphql"

func (a *App) Service(name string) *Service {
	a.registry.RegisterService(registry.Service{Name: name})
	return &Service{app: a, name: name, graphqlPath: DefaultGraphQLPath}
}

// AtGraphQL overrides the GraphQL mount path for this service. Most apps
// keep the default ("/graphql") — override only when you need a
// per-service path (e.g. "/admin/graphql") or want to unify multiple
// nexus apps behind the same reverse proxy.
//
//	app.Service("admin").AtGraphQL("/admin/graphql").Describe("Admin ops")
func (s *Service) AtGraphQL(path string) *Service {
	if path == "" {
		path = DefaultGraphQLPath
	}
	s.graphqlPath = path
	return s
}

// Name returns the service's identifier (same string used on the dashboard
// and passed to *App.Service). Exposed so framework internals (the auto-
// mount Invoke, service wrappers) can identify a service without reaching
// into the private field.
func (s *Service) Name() string { return s.name }

// GraphQLPath returns the mount path set via AtGraphQL (or the default).
// Read by the auto-mount Invoke; users rarely need this.
func (s *Service) GraphQLPath() string { return s.graphqlPath }

// Auth wires a Bearer-token → user hook. Resolvers read the user via
// graph.GetRootInfo(p, "details", &user) after successful authentication.
// Per-service because different services often use different auth
// mechanisms (admin vs public).
func (s *Service) Auth(fn UserDetailsFn) *Service { s.graphqlUserDetFn = fn; return s }

// graphqlOptions returns the gql.Option slice representing this service's
// current flags combined with the app-wide knobs in cfg. Called by the
// auto-mount.
func (s *Service) graphqlOptions(cfg Config) []gql.Option {
	var out []gql.Option
	if !cfg.DisablePlayground {
		out = append(out, gql.WithPlayground(true))
	}
	if cfg.GraphQLDebug {
		out = append(out, gql.WithDEBUG(true))
	}
	if cfg.GraphQLPretty {
		out = append(out, gql.WithPretty(true))
	}
	if s.graphqlUserDetFn != nil {
		fn := s.graphqlUserDetFn
		out = append(out, gql.WithUserDetailsFn(func(ctx context.Context, token string) (context.Context, any, error) {
			return fn(ctx, token)
		}))
	}
	return out
}

func (s *Service) Describe(desc string) *Service {
	s.app.registry.RegisterService(registry.Service{Name: s.name, Description: desc})
	return s
}

func (s *Service) REST(method, path string) *rest.Builder {
	return rest.New(s.app.engine, s.app.registry, s.app.bus, s.name, method, path)
}

func (s *Service) WebSocket(path string) *ws.Builder {
	return ws.New(s.app.engine, s.app.registry, s.app.bus, s.name, path)
}

// MountGraphQL attaches schema (assembled by go-graph or graphql-go) and
// auto-registers every operation into the nexus registry. Pass gql.With*
// options for auth (UserDetailsFn), Playground, Pretty, and DEBUG.
func (s *Service) MountGraphQL(path string, schema *graphql.Schema, opts ...gql.Option) {
	gql.Mount(s.app.engine, s.app.registry, s.app.bus, s.name, path, schema, opts...)
}

// Attach links a resource to this service so the dashboard draws an edge.
// If the resource isn't already registered, Attach registers it too — that's
// convenient for ad-hoc services but means typos silently create orphan nodes.
// For centrally-declared resources, prefer .Using("name") instead.
func (s *Service) Attach(r resource.Resource) *Service {
	s.app.registry.RegisterResource(r)
	s.app.registry.AttachResource(s.name, r.Name())
	return s
}

// Using attaches already-registered resources by name so the dashboard draws
// edges. An empty string resolves to the default database (the resource of
// kind Database marked resource.AsDefault(), or the lexically-first if none
// is marked). Unknown names are attached anyway so the registry shows a
// disconnected edge — surfacing the typo rather than hiding it.
//
//	app.Service("adverts").Using("").MountGraphQL(...)               // default DB
//	app.Service("qb").Using("questions", "session").MountGraphQL(...) // explicit
func (s *Service) Using(names ...string) *Service {
	for _, name := range names {
		if name == "" {
			r := s.app.registry.DefaultOfKind(resource.KindDatabase)
			if r == nil {
				continue
			}
			name = r.Name()
		}
		s.app.registry.AttachResource(s.name, name)
	}
	return s
}

// UsingDefaults attaches the default resource of every kind that has at least
// one registered (database, cache, queue). Useful for services that touch the
// common "main DB + session cache" pair without naming either.
func (s *Service) UsingDefaults() *Service {
	for _, kind := range []resource.Kind{resource.KindDatabase, resource.KindCache, resource.KindQueue} {
		if r := s.app.registry.DefaultOfKind(kind); r != nil {
			s.app.registry.AttachResource(s.name, r.Name())
		}
	}
	return s
}