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
	// Module is the nexus.Module("name", ...) this endpoint was declared
	// inside. Empty when registered outside any module (top-level opt to
	// Run/New). Drives the architecture-view grouping: module containers
	// enclose their endpoints; services are depicted as separate dep
	// nodes the endpoints connect to.
	Module string `json:",omitempty"`
	// Deployment is the nexus.DeployAs(tag) marker on the enclosing
	// module — it names the unit a future split would peel off. Empty
	// for modules that are always local. Surfaces on the dashboard so
	// readers can see which endpoints belong to a planned (or current)
	// deployment unit.
	Deployment   string `json:",omitempty"`
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
	// Deployment carries the DeployAs tag of the module that owns this
	// service (if any). When the service has no owning module — or
	// the module isn't tagged — this is empty.
	Deployment string `json:",omitempty"`
	// Remote marks a service that lives in a different deployment
	// unit. Set by the shadow generator's RemoteService option so
	// the dashboard can render peer services distinctively (a
	// "remote" badge, ghosted card style) rather than mixing them
	// with the locally-owned services.
	Remote bool `json:",omitempty"`
	// ResourceDeps is the set of resource names the service's
	// CONSTRUCTOR depends on — i.e. NewXService(app, db *DBManager,
	// ...) records "db"'s NexusResources here. These drive
	// service-level architecture edges (service → resource) in the
	// dashboard, separate from per-endpoint edges which come from
	// each handler's own deps.
	ResourceDeps []string `json:",omitempty"`
	// ServiceDeps is the set of OTHER service names the constructor
	// depends on — detected when the constructor takes another
	// service wrapper as a parameter. Renders as service → service
	// edges on the architecture graph.
	ServiceDeps []string `json:",omitempty"`
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

// Worker describes a long-lived background task registered via
// nexus.AsWorker. The Dashboard renders workers as a separate node
// kind on the architecture view — like a service, they have
// resource/service deps (inferred from the worker func's params),
// but they don't handle HTTP traffic. Status reflects the current
// lifecycle stage; LastError carries the message from a panic or a
// non-nil error return.
type Worker struct {
	Name         string
	Status       string // "starting" | "running" | "stopped" | "failed"
	Description  string `json:",omitempty"`
	StartedAt    time.Time
	StoppedAt    time.Time `json:",omitempty"`
	LastError    string    `json:",omitempty"`
	ResourceDeps []string  `json:",omitempty"`
	ServiceDeps  []string  `json:",omitempty"`
}

type Registry struct {
	mu          sync.RWMutex
	services    map[string]Service
	endpoints   []Endpoint
	resources   map[string]resource.Resource
	attached    map[string][]string // resource name -> service names
	middlewares map[string]middleware.Info
	workers     map[string]Worker
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
		workers:     map[string]Worker{},
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
	if ok {
		if s.Description == "" {
			s.Description = existing.Description
		}
		if len(s.ResourceDeps) == 0 {
			s.ResourceDeps = existing.ResourceDeps
		}
		if len(s.ServiceDeps) == 0 {
			s.ServiceDeps = existing.ServiceDeps
		}
		// A later local registration must not silently demote an
		// already-registered remote placeholder back to local; let
		// whichever side called LAST win on the Remote bit, but keep
		// the local-side wins-on-name semantics for everything else.
		// In practice the shadow's RemoteService Invoke runs at fx
		// boot — same time as local registrations — so the explicit
		// new-value semantic is right.
	}
	r.services[s.Name] = s
}

// RegisterWorker records a new worker entry. If a worker with the
// same name already exists, its metadata (deps, description) is
// preserved but its Status is reset — typically called when fx
// constructs a new instance on app restart within a test.
func (r *Registry) RegisterWorker(w Worker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing := r.workers[w.Name]
	if w.Description == "" {
		w.Description = existing.Description
	}
	if len(w.ResourceDeps) == 0 {
		w.ResourceDeps = existing.ResourceDeps
	}
	if len(w.ServiceDeps) == 0 {
		w.ServiceDeps = existing.ServiceDeps
	}
	if w.Status == "" {
		w.Status = "starting"
	}
	r.workers[w.Name] = w
}

// UpdateWorkerStatus sets a worker's Status and optional LastError.
// Timestamps (StartedAt / StoppedAt) are filled in based on
// status transitions so the dashboard can surface runtime durations.
func (r *Registry) UpdateWorkerStatus(name, status, lastError string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.workers[name]
	if !ok {
		return
	}
	prev := w.Status
	w.Status = status
	if lastError != "" {
		w.LastError = lastError
	}
	now := time.Now()
	switch status {
	case "running":
		if prev != "running" {
			w.StartedAt = now
		}
	case "stopped", "failed":
		w.StoppedAt = now
	}
	r.workers[name] = w
}

// Workers returns a snapshot of registered workers sorted by name.
func (r *Registry) Workers() []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Worker, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SetServiceDeps records the resource + service dependencies of a
// service's constructor. Called by nexus.ProvideService after fx has
// resolved the constructor's params, so we know which resources
// (NexusResourceProvider) and services (service-wrapper types) were
// injected. Replaces any previously-recorded deps for the service.
func (r *Registry) SetServiceDeps(name string, resourceDeps, serviceDeps []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.services[name]
	s.Name = name
	s.ResourceDeps = dedupeSort(resourceDeps)
	s.ServiceDeps = dedupeSort(serviceDeps)
	r.services[name] = s
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

// GetService returns the registered service entry for name, or
// (zero, false) when no service of that name exists yet. Used by
// the framework's cross-module-dep merge path to read-modify-write
// ServiceDeps without losing existing fields.
func (r *Registry) GetService(name string) (Service, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.services[name]
	return s, ok
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

// SetEndpointModule records the nexus.Module name this endpoint was
// declared inside. Called by the auto-mount (GraphQL) and by the REST
// invoke path after an endpoint is registered. A later call overwrites
// an earlier one so nested Module(...) wrappers resolve innermost-wins.
func (r *Registry) SetEndpointModule(service, name, module string) {
	if module == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].Module = module
			return
		}
	}
}

// SetEndpointDeployment records the nexus.DeployAs tag for an endpoint.
// Called by the auto-mount path (GraphQL) — REST/WS invokes stamp the
// field directly at RegisterEndpoint time. Empty tag is a no-op.
func (r *Registry) SetEndpointDeployment(service, name, deployment string) {
	if deployment == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.endpoints {
		if r.endpoints[i].Service == service && r.endpoints[i].Name == name {
			r.endpoints[i].Deployment = deployment
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