// Package registry stores metadata about every endpoint a nexus app exposes.
// It is the source of truth the dashboard reads from.
package registry

import (
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/paulmanoni/nexus/middleware"
	"github.com/paulmanoni/nexus/resource"
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

	// Resources is the list of named resources this endpoint touches,
	// derived at mount time from the handler's dep list (each dep that
	// implements NexusResourceProvider contributes its NexusResources()
	// entries). Surfaces on the dashboard as per-op chips so a reader
	// can see, for each resolver, exactly which DB/cache/queue it hits
	// without tracing through Go code.
	Resources []string `json:",omitempty"`

	// ServiceDeps is the list of OTHER services this endpoint calls into,
	// derived from any handler dep that unwraps to a *Service (e.g. a
	// *UsersService embedded wrapper). Drives per-op service→service
	// edges on the dashboard. The owning service itself is excluded —
	// no self-loops.
	ServiceDeps []string `json:",omitempty"`

	// ServiceAutoRouted is true when the handler didn't explicitly name
	// its owning service (no service-wrapper dep, no OnService option)
	// and the auto-mount adopted it into the app's single service at
	// startup. The dashboard renders an owner chip on rows where this is
	// true so readers can tell implicit placement from explicit.
	ServiceAutoRouted bool `json:",omitempty"`

	// RateLimit is the declared rate limit for this endpoint, if any.
	// Dashboard surfaces RPM/burst/per-IP as a chip; operators can
	// override the effective limit live via /__nexus/ratelimits.
	RateLimit *RateLimitInfo `json:",omitempty"`

	// GraphQL-specific metadata, populated when Transport == GraphQL.
	Args       []GraphQLArg `json:",omitempty"`
	ReturnType string       `json:",omitempty"`

	// Deprecation flows from graph.WithDeprecated on the underlying resolver.
	Deprecated        bool   `json:",omitempty"`
	DeprecationReason string `json:",omitempty"`
}

// RateLimitInfo is the registry-serializable shape of a rate limit for
// dashboard display. Mirrors ratelimit.Limit but JSON-tagged for the UI
// plus an Overridden flag so a single GET can surface both "what the
// code says" and "what the operator tuned it to."
type RateLimitInfo struct {
	RPM        int  `json:"rpm"`
	Burst      int  `json:"burst,omitempty"`
	PerIP      bool `json:"perIP,omitempty"`
	Overridden bool `json:"overridden,omitempty"`
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
	middlewares map[string]middleware.Info
	// globalMiddlewares is the ordered list of middleware names
	// installed on the engine root (app-wide). Every request goes
	// through these before per-endpoint stacks. Dashboard renders them
	// as a strip so operators see the global pre-gate at a glance.
	globalMiddlewares []string
}

func New() *Registry {
	r := &Registry{
		services:    map[string]Service{},
		resources:   map[string]resource.Resource{},
		attached:    map[string][]string{},
		middlewares: map[string]middleware.Info{},
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
func (r *Registry) RegisterMiddleware(m middleware.Info) {
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
	r.middlewares[name] = middleware.Info{Name: name, Kind: middleware.KindCustom}
}

// RegisterGlobalMiddleware marks name as installed at engine root.
// Order matters — same order as Config.GlobalMiddleware + built-in
// extras — so the dashboard strip reads left-to-right as the request
// encounters them. Safe to call multiple times with the same name;
// later calls become no-ops.
func (r *Registry) RegisterGlobalMiddleware(name string) {
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.globalMiddlewares {
		if existing == name {
			return
		}
	}
	r.globalMiddlewares = append(r.globalMiddlewares, name)
}

// GlobalMiddlewares returns the ordered list of global middleware
// names. Dashboard reads this to render the top-of-canvas strip.
func (r *Registry) GlobalMiddlewares() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.globalMiddlewares))
	copy(out, r.globalMiddlewares)
	return out
}

// Middlewares returns the middleware metadata snapshot, sorted by name.
func (r *Registry) Middlewares() []middleware.Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]middleware.Info, 0, len(r.middlewares))
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

// SetEndpointResources records the resources a specific endpoint touches.
// Dedupes and sorts so chip rendering is deterministic. Match is on
// (service, name) — unique per GraphQL op, and per method+path for REST.
func (r *Registry) SetEndpointResources(service, name string, resources []string) {
	uniq := dedupeSort(resources)
	if len(uniq) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].Resources = uniq
			return
		}
	}
}

// SetEndpointServiceDeps records the OTHER services this endpoint calls
// into. Shape-parallel with SetEndpointResources so the dashboard can
// draw per-op edges to services the same way it draws resource edges.
func (r *Registry) SetEndpointServiceDeps(service, name string, services []string) {
	uniq := dedupeSort(services)
	if len(uniq) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].ServiceDeps = uniq
			return
		}
	}
}

// SetEndpointServiceAutoRouted flags an endpoint as having been auto-adopted
// into its service by the auto-mount (the handler didn't declare the
// owning service). Dashboard uses it to render an owner chip only on
// implicitly-routed rows.
func (r *Registry) SetEndpointServiceAutoRouted(service, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].ServiceAutoRouted = true
			return
		}
	}
}

// SetEndpointRateLimit attaches rate-limit info to an endpoint. Called
// after mount when an op declared RateLimit(...); subsequent dashboard
// overrides are reflected separately via the ratelimit.Store snapshot.
func (r *Registry) SetEndpointRateLimit(service, name string, info RateLimitInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].RateLimit = &info
			return
		}
	}
}

// dedupeSort returns the unique entries of xs sorted ascending. Empty
// input returns a nil slice so callers can skip the registry lock.
func dedupeSort(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if _, ok := seen[x]; ok {
			continue
		}
		seen[x] = struct{}{}
		out = append(out, x)
	}
	sort.Strings(out)
	return out
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