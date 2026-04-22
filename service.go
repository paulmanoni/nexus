package nexus

import (
	"github.com/graphql-go/graphql"

	"nexus/registry"
	"nexus/resource"
	"nexus/transport/gql"
	"nexus/transport/rest"
	"nexus/transport/ws"
)

// Service is a named group of endpoints. Services are the nodes the dashboard
// draws in the architecture view.
type Service struct {
	app  *App
	name string
}

func (a *App) Service(name string) *Service {
	a.registry.RegisterService(registry.Service{Name: name})
	return &Service{app: a, name: name}
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
// auto-registers every operation into the nexus registry.
func (s *Service) MountGraphQL(path string, schema *graphql.Schema) {
	gql.Mount(s.app.engine, s.app.registry, s.app.bus, s.name, path, schema)
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