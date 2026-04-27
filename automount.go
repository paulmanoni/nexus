package nexus

import (
	"fmt"
	"reflect"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/graph"
	"github.com/paulmanoni/nexus/metrics"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
	"github.com/paulmanoni/nexus/transport/gql"
)

// autoMountIn is the fx.In for the shared-group consumer. We pull every
// GqlField produced by nexus.AsQuery / AsMutation, the Config (for the
// environment-level GraphQL knobs), and the app itself, then partition by
// service type and build one schema per service.
type autoMountIn struct {
	fx.In
	App    *App
	Cfg    Config
	Fields []GqlField `group:"nexus.graph.fields"`
}

// autoMountGraphQL runs once at fx.Start, after every reflective controller
// constructor has resolved. Collapsing everything into a single function
// means users write no mount ceremony — service wrapper + AsQuery/AsMutation
// is all they need.
func autoMountGraphQL(in autoMountIn) error {
	if len(in.Fields) == 0 {
		return nil
	}

	// First pass: discover which service instances exist among the resolved
	// fields. Any unresolved entry (ServiceType==nil) inherits the lone
	// resolved service; if multiple services are in play, unresolved
	// entries are ambiguous and the app can't boot.
	var distinctType reflect.Type
	var distinctService *Service
	distinctCount := 0
	hasUnresolved := false
	seenTypes := map[reflect.Type]bool{}
	for _, f := range in.Fields {
		if f.ServiceType == nil || f.Service == nil {
			hasUnresolved = true
			continue
		}
		if !seenTypes[f.ServiceType] {
			seenTypes[f.ServiceType] = true
			distinctCount++
			distinctType = f.ServiceType
			distinctService = f.Service
		}
	}
	// Zero-service fallback: when the app registered no services but has
	// handlers without a *Service dep, synthesize a default service so the
	// schema still mounts. Lets minimal apps skip service modeling entirely
	// (single /graphql endpoint, handlers are plain funcs with deps).
	if hasUnresolved && distinctCount == 0 {
		distinctService = in.App.Service(defaultServiceName)
		distinctType = reflect.TypeFor[*Service]()
		distinctCount = 1
	}
	resolveUnresolved := func(f GqlField) (GqlField, error) {
		if f.ServiceType != nil && f.Service != nil {
			return f, nil
		}
		if distinctCount == 1 {
			f.ServiceType = distinctType
			f.Service = distinctService
			return f, nil
		}
		return f, fmt.Errorf("nexus: handler lacks a *Service dep and the app has %d services — add one to the signature or use nexus.OnService[*Svc]()", distinctCount)
	}

	// Partition by service type. The service instance is already unwrapped
	// inside each GqlField (AsQuery did this via the service-wrapper dep
	// scan or OnService option) so we can read path/name directly.
	partitions := map[reflect.Type]*servicePartition{}
	// Stash per-op resource/service-dep lists during the walk; applied
	// AFTER mountOne so the registry.Set* helpers have endpoints to match.
	type pending struct {
		service, op string
		resources   []string
		serviceDeps []string
	}
	var pendingEdges []pending

	// pendingModules tracks (service, op, module, deployment) tuples
	// gathered during the walk. Applied AFTER mountOne so endpoints
	// exist in the registry. Collected here (not in a separate pass)
	// because f.Service is filled by resolveUnresolved inside this
	// loop — a second pass would see the pre-resolution nil for
	// zero-service fallback fields.
	type pendingModule struct{ service, op, module, deployment string }
	var pendingModules []pendingModule

	// autoRouted tracks (service, op) pairs whose service was filled in by
	// resolveUnresolved — these get an owner chip on the dashboard.
	type opKey struct{ service, op string }
	autoRouted := map[opKey]bool{}

	for _, f := range in.Fields {
		wasUnresolved := f.ServiceType == nil || f.Service == nil
		resolved, err := resolveUnresolved(f)
		if err != nil {
			return err
		}
		f = resolved
		p, ok := partitions[f.ServiceType]
		if !ok {
			p = &servicePartition{serviceType: f.ServiceType, service: f.Service}
			partitions[f.ServiceType] = p
		}
		switch f.Kind {
		case graph.FieldKindQuery:
			if qf, ok := f.Field.(graph.QueryField); ok {
				p.queries = append(p.queries, qf)
			}
		case graph.FieldKindMutation:
			if mf, ok := f.Field.(graph.MutationField); ok {
				p.mutations = append(p.mutations, mf)
			}
		}
		attachDeclaredResources(in.App, f)

		// Auto-attach the metrics recorder now that the service name is
		// resolved. Done here rather than in asGqlField because auto-
		// routed ops don't know their service at registration time —
		// here we do, so the counter key is always "<service>.<op>".
		if u, ok := f.Field.(*graph.UnifiedResolver[any]); ok {
			info, ok := graph.Inspect(f.Field)
			if ok && f.Service != nil {
				key := f.Service.Name() + "." + info.Name
				mw := metrics.NewMiddleware(in.App.metricsStore, key)
				u.WithNamedMiddleware(mw.Name, mw.Description, mw.Graph)
				in.App.registry.RegisterMiddleware(mw.AsInfo())
			}
		}

		// Collect every external surface this op touches:
		//   - resources via NexusResourceProvider deps (DBs, caches, queues)
		//   - other services via *Service-embedding deps
		// Self-loops are filtered so the owning service doesn't edge-to-itself.
		if info, ok := graph.Inspect(f.Field); ok && f.Service != nil {
			resources := collectResourceNames(f.Deps)
			services := collectServiceDeps(f.Deps, f.DepTypes, f.Service.Name())
			if len(resources) > 0 || len(services) > 0 {
				pendingEdges = append(pendingEdges, pending{
					service:     f.Service.Name(),
					op:          info.Name,
					resources:   resources,
					serviceDeps: services,
				})
			}
			if wasUnresolved {
				autoRouted[opKey{service: f.Service.Name(), op: info.Name}] = true
			}
			if f.Module != "" || f.Deployment != "" {
				pendingModules = append(pendingModules, pendingModule{
					service:    f.Service.Name(),
					op:         info.Name,
					module:     f.Module,
					deployment: f.Deployment,
				})
			}
		}
	}

	// Group partitions by mount path before calling gql.Mount: when N
	// services share the same /graphql path, mounting per-service
	// would double-register POST/GET on the engine and panic. Merge
	// their query/mutation lists into one schema per path so each
	// path lands a single Mount call.
	pathGroups := []*pathGroup{}
	pathByMount := map[string]*pathGroup{}
	for _, p := range partitions {
		if p.service == nil {
			return fmt.Errorf("nexus: first dep of a %s handler must be a *Service wrapper (got %s)",
				p.serviceType, p.serviceType)
		}
		path := p.service.GraphQLPath()
		g, ok := pathByMount[path]
		if !ok {
			g = &pathGroup{
				path:  path,
				owner: p.service,
				opts:  p.service.graphqlOptions(in.Cfg),
			}
			pathByMount[path] = g
			pathGroups = append(pathGroups, g)
		}
		g.partitions = append(g.partitions, p)
	}
	for _, g := range pathGroups {
		if err := mountGroup(in.App, g); err != nil {
			return err
		}
	}

	// Endpoints are now in the registry — apply per-op edge records and
	// mark any auto-routed ops so the dashboard can distinguish them.
	for _, pr := range pendingEdges {
		in.App.Registry().SetEndpointResources(pr.service, pr.op, pr.resources)
		in.App.Registry().SetEndpointServiceDeps(pr.service, pr.op, pr.serviceDeps)
	}
	for k := range autoRouted {
		in.App.Registry().SetEndpointServiceAutoRouted(k.service, k.op)
	}

	// Stamp the module name onto each endpoint so the architecture view
	// can group endpoints by module container. Collected during the main
	// field walk (see pendingModules) so the service name reflects the
	// resolveUnresolved outcome rather than the pre-resolution f.Service.
	for _, pm := range pendingModules {
		in.App.Registry().SetEndpointModule(pm.service, pm.op, pm.module)
		in.App.Registry().SetEndpointDeployment(pm.service, pm.op, pm.deployment)
	}

	// Publish declared rate limits to the registry so the dashboard can
	// render them alongside the endpoint metadata. Live overrides live
	// in the store and are surfaced by GET /__nexus/ratelimits.
	for _, f := range in.Fields {
		if f.RateLimit == nil || f.Service == nil {
			continue
		}
		info, ok := graph.Inspect(f.Field)
		if !ok {
			continue
		}
		in.App.Registry().SetEndpointRateLimit(f.Service.Name(), info.Name, registry.RateLimitInfo{
			RPM:   f.RateLimit.RPM,
			Burst: f.RateLimit.EffectiveBurst(),
			PerIP: f.RateLimit.PerIP,
		})
	}
	return nil
}

// pathGroup bundles every partition that mounts at the same GraphQL
// path. autoMountGraphQL groups partitions through this structure so
// schemas merge instead of double-registering routes.
type pathGroup struct {
	path       string
	owner      *Service // first service on this path — names the registered endpoint
	opts       []gql.Option
	partitions []*servicePartition
}

type servicePartition struct {
	serviceType reflect.Type
	service     *Service
	queries     []graph.QueryField
	mutations   []graph.MutationField
}

// mountGroup builds one merged schema for every partition that shares
// the group's GraphQL path and mounts it with a single gql.Mount
// call. The owner service (the first partition seen on this path)
// names the registered endpoint — subsequent services contribute
// queries / mutations into the same schema. Endpoint registry
// patching runs per-partition so each query/mutation still records
// against its own service for the dashboard.
func mountGroup(app *App, g *pathGroup) error {
	var queries []graph.QueryField
	var mutations []graph.MutationField
	for _, p := range g.partitions {
		queries = append(queries, p.queries...)
		mutations = append(mutations, p.mutations...)
	}
	if len(queries) == 0 && len(mutations) == 0 {
		return nil
	}
	schema, err := graph.NewSchemaBuilder(graph.SchemaBuilderParams{
		QueryFields:    queries,
		MutationFields: mutations,
	}).Build()
	if err != nil {
		return fmt.Errorf("nexus: build schema for path %q: %w", g.path, err)
	}
	gql.Mount(app.Engine(), app.Registry(), app.Bus(), g.owner.Name(), g.path, &schema, g.opts...)

	for _, p := range g.partitions {
		for _, q := range p.queries {
			patchRegistryFromField(app.Registry(), p.service.Name(), q)
		}
		for _, m := range p.mutations {
			patchRegistryFromField(app.Registry(), p.service.Name(), m)
		}
	}
	return nil
}

// NexusResourceProvider is implemented by managers that know the external
// resources they front. A manager's NexusResources slice is used in two
// places:
//
//  1. Boot-time registration via nexus.ProvideResources — each returned
//     resource.Resource is added to the app registry so it appears on the
//     dashboard with its health, description, and details.
//  2. Service attachment via the GraphQL auto-mount — whenever a resolver
//     names this manager as a dep, every resource in the slice gets linked
//     to the owning service by name, drawing the architecture edge.
//
// A manager may list more resources than any one handler uses; the edge is
// drawn per named resource on every service that mentions the manager.
//
//	func (m *DBManager) NexusResources() []resource.Resource {
//	    var out []resource.Resource
//	    m.Each(func(name string, db *DB) {
//	        out = append(out, resource.NewDatabase(name, ...))
//	    })
//	    return out
//	}
type NexusResourceProvider interface {
	NexusResources() []resource.Resource
}

func attachDeclaredResources(app *App, f GqlField) {
	if f.Service == nil {
		return
	}
	for _, dep := range f.Deps {
		if !dep.IsValid() {
			continue
		}
		provider, ok := dep.Interface().(NexusResourceProvider)
		if !ok {
			continue
		}
		for _, r := range provider.NexusResources() {
			app.Registry().AttachResource(f.Service.Name(), r.Name())
		}
	}
}

// collectServiceDeps returns the names of OTHER services this handler
// calls into — any dep that unwraps to a *Service. Skips the owning
// service (first dep convention) so self-loops never appear on the graph.
// Example: a handler declared `func(svc *AdvertsService, users *UsersService, ...)`
// yields ["users"], giving the dashboard an adverts→users edge.
func collectServiceDeps(deps []reflect.Value, depTypes []reflect.Type, owning string) []string {
	var out []string
	for i, dep := range deps {
		if !dep.IsValid() || i >= len(depTypes) {
			continue
		}
		svc, ok := unwrapService(dep, depTypes[i])
		if !ok || svc == nil {
			continue
		}
		if svc.name == owning {
			continue
		}
		out = append(out, svc.name)
	}
	return out
}

// collectResourceNames flattens every NexusResourceProvider dep into a
// deduplicated list of resource names. Kept separate from the attach path
// so a future caller can reuse it without double-attaching.
func collectResourceNames(deps []reflect.Value) []string {
	var names []string
	for _, dep := range deps {
		if !dep.IsValid() {
			continue
		}
		provider, ok := dep.Interface().(NexusResourceProvider)
		if !ok {
			continue
		}
		for _, r := range provider.NexusResources() {
			names = append(names, r.Name())
		}
	}
	return names
}

func patchRegistryFromField(reg *registry.Registry, service string, f any) {
	info, ok := graph.Inspect(f)
	if !ok {
		return
	}
	for _, mw := range info.Middlewares {
		if mw.Name != "" && mw.Name != "anonymous" {
			reg.EnsureMiddleware(mw.Name)
		}
	}
	reg.UpdateGraphQLEndpoint(service, info.Name, registry.GraphQLUpdate{
		Description:       info.Description,
		ReturnType:        info.ReturnType,
		Args:              convertArgInfos(info.Args),
		Middleware:        middlewareNames(info.Middlewares),
		Deprecated:        info.Deprecated,
		DeprecationReason: info.DeprecationReason,
	})
}

func convertArgInfos(args []graph.ArgInfo) []registry.GraphQLArg {
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
