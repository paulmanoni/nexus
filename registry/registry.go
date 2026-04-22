// Package registry stores metadata about every endpoint a nexus app exposes.
// It is the source of truth the dashboard reads from.
package registry

import (
	"slices"
	"sort"
	"sync"
	"time"

	"nexus/middleware"
	"nexus/resource"
)

type Transport string

const (
	REST      Transport = "rest"
	GraphQL   Transport = "graphql"
	WebSocket Transport = "websocket"
)

type Endpoint struct {
	Service      string
	Name         string
	Transport    Transport
	Method       string // REST verb; GraphQL "query"/"mutation"/"subscription"; unused for WS
	Path         string // REST/WS path; for GraphQL this is the mount path (e.g. "/graphql")
	Description  string
	Middleware   []string
	Tags         map[string]string
	RegisteredAt time.Time

	// GraphQL-specific metadata, populated when Transport == GraphQL.
	Args       []GraphQLArg `json:",omitempty"`
	ReturnType string       `json:",omitempty"`

	// Deprecation flows from graph.WithDeprecated on the underlying resolver.
	Deprecated        bool   `json:",omitempty"`
	DeprecationReason string `json:",omitempty"`
}

// GraphQLArg describes one argument on a GraphQL field. Used by the dashboard
// to pre-fill the tester's query template with typed variables and render
// validator chips.
type GraphQLArg struct {
	Name        string
	Type        string // SDL-style, e.g. "String!", "[Int]", "UUID"
	Description string             `json:",omitempty"`
	Required    bool               `json:",omitempty"`
	Default     any                `json:"Default,omitempty"`
	Validators  []GraphQLValidator `json:",omitempty"`
}

// GraphQLValidator mirrors graph.ValidatorInfo — what kind of rule applies,
// the user-facing message, and any parameters (e.g. min/max for length).
type GraphQLValidator struct {
	Kind    string
	Message string `json:",omitempty"`
	Details any    `json:",omitempty"`
}

// GraphQLUpdate is the patch graphfx applies to an endpoint after it has
// introspected the go-graph resolver. Only fields set on the update overwrite
// the endpoint; others are preserved.
type GraphQLUpdate struct {
	Description       string
	ReturnType        string
	Args              []GraphQLArg
	Middleware        []string
	Deprecated        bool
	DeprecationReason string
}

type Service struct {
	Name        string
	Description string
}

// ResourceSnapshot is the serializable view of a resource at a point in time.
// The dashboard polls this via /__nexus/resources — healthy is probed fresh
// on every snapshot so the UI reflects live state.
type ResourceSnapshot struct {
	Name        string         `json:"name"`
	Kind        resource.Kind  `json:"kind"`
	Description string         `json:"description,omitempty"`
	Healthy     bool           `json:"healthy"`
	Details     map[string]any `json:"details,omitempty"`
	AttachedTo  []string       `json:"attachedTo,omitempty"`
}

type Registry struct {
	mu          sync.RWMutex
	services    map[string]Service
	endpoints   []Endpoint
	resources   map[string]resource.Resource
	attached    map[string][]string // resource name -> service names
	middlewares map[string]middleware.Middleware
}

func New() *Registry {
	r := &Registry{
		services:    map[string]Service{},
		resources:   map[string]resource.Resource{},
		attached:    map[string][]string{},
		middlewares: map[string]middleware.Middleware{},
	}
	for _, m := range middleware.Builtins {
		r.middlewares[m.Name] = m
	}
	return r
}

func (r *Registry) RegisterService(s Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.services[s.Name]
	if ok && s.Description == "" {
		s.Description = existing.Description
	}
	r.services[s.Name] = s
}

func (r *Registry) RegisterEndpoint(e Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e.RegisteredAt.IsZero() {
		e.RegisteredAt = time.Now()
	}
	r.endpoints = append(r.endpoints, e)
}

func (r *Registry) Endpoints() []Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Endpoint, len(r.endpoints))
	copy(out, r.endpoints)
	return out
}

func (r *Registry) Services() []Service {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Service, 0, len(r.services))
	for _, s := range r.services {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) EndpointsByService(name string) []Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Endpoint
	for _, e := range r.endpoints {
		if e.Service == name {
			out = append(out, e)
		}
	}
	return out
}

// RegisterResource adds a resource to the registry. Safe to call multiple
// times with the same resource — later calls overwrite earlier ones.
func (r *Registry) RegisterResource(res resource.Resource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources[res.Name()] = res
}

// AttachResource links a resource to a service. The dashboard draws an edge
// service → resource. Multiple services may attach to the same resource.
func (r *Registry) AttachResource(serviceName, resourceName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.attached[resourceName], serviceName) {
		return
	}
	r.attached[resourceName] = append(r.attached[resourceName], serviceName)
}

// RegisterMiddleware adds or overwrites a middleware entry. Safe to call at
// any time; the dashboard reflects the latest on its next poll.
func (r *Registry) RegisterMiddleware(m middleware.Middleware) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.middlewares[m.Name] = m
}

// EnsureMiddleware registers an unknown name as a Custom middleware. Builders
// call this whenever .Use("name", fn) runs so the dashboard surfaces every
// middleware name the app references, even those the user never declared
// explicitly. Does nothing when the name already exists (preserves builtin).
func (r *Registry) EnsureMiddleware(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.middlewares[name]; ok {
		return
	}
	r.middlewares[name] = middleware.Middleware{Name: name, Kind: middleware.KindCustom}
}

// Middlewares returns the middleware metadata snapshot, sorted by name.
func (r *Registry) Middlewares() []middleware.Middleware {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]middleware.Middleware, 0, len(r.middlewares))
	for _, m := range r.middlewares {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// UpdateGraphQLEndpoint patches an existing GraphQL endpoint with richer
// metadata from go-graph introspection. Non-empty fields on the update
// overwrite the endpoint; empty fields are left alone (so a follow-up call
// only carrying middleware names won't wipe previously-set args).
func (r *Registry) UpdateGraphQLEndpoint(service, name string, u GraphQLUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		e := &r.endpoints[i]
		if e.Service != service || e.Transport != GraphQL || e.Name != name {
			continue
		}
		if u.Description != "" {
			e.Description = u.Description
		}
		if u.ReturnType != "" {
			e.ReturnType = u.ReturnType
		}
		if u.Args != nil {
			e.Args = u.Args
		}
		if u.Middleware != nil {
			e.Middleware = u.Middleware
		}
		if u.Deprecated {
			e.Deprecated = true
			e.DeprecationReason = u.DeprecationReason
		}
		return
	}
}

// SetEndpointMiddlewares applies a middleware chain to every registered
// endpoint matching (service, transport). Used by graphfx to tag a whole
// GraphQL mount with the middleware that actually applied inside go-graph.
func (r *Registry) SetEndpointMiddlewares(service string, transport Transport, names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Transport == transport {
			r.endpoints[i].Middleware = append([]string(nil), names...)
		}
	}
}

// FindResource looks a resource up by name. Returns nil if unknown.
func (r *Registry) FindResource(name string) resource.Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.resources[name]
}

// DefaultOfKind returns the resource marked resource.AsDefault() for the
// kind, or — if none is marked — the lexically-first registered resource of
// that kind. Returns nil when no resources of the kind exist.
func (r *Registry) DefaultOfKind(kind resource.Kind) resource.Resource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var first resource.Resource
	for _, res := range r.resources {
		if res.Kind() != kind {
			continue
		}
		if res.IsDefault() {
			return res
		}
		if first == nil || res.Name() < first.Name() {
			first = res
		}
	}
	return first
}

// Resources returns a fresh snapshot with health probed right now.
func (r *Registry) Resources() []ResourceSnapshot {
	r.mu.RLock()
	resources := make([]resource.Resource, 0, len(r.resources))
	for _, res := range r.resources {
		resources = append(resources, res)
	}
	attached := make(map[string][]string, len(r.attached))
	for k, v := range r.attached {
		cp := make([]string, len(v))
		copy(cp, v)
		attached[k] = cp
	}
	r.mu.RUnlock()

	out := make([]ResourceSnapshot, 0, len(resources))
	for _, res := range resources {
		out = append(out, ResourceSnapshot{
			Name:        res.Name(),
			Kind:        res.Kind(),
			Description: res.Describe(),
			Healthy:     res.Healthy(),
			Details:     res.Details(),
			AttachedTo:  attached[res.Name()],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}