package nexus

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/manifest"
	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
)

// manifestStore holds every declaration registered against an *App by
// the option helpers below (DeclareEnv, DeclareService, UseVolume,
// AddStartupTask) plus the corresponding *Provider variants. All
// access goes through manifestMu so concurrent invokes don't corrupt
// the slices — fx invokes are sequential today but it's cheap
// insurance and keeps the public methods safe to call from anywhere.
//
// The store is intentionally additive-only. There's no Unregister:
// once a module has declared its env vars / service needs, those
// declarations are part of the app's identity for the lifetime of
// the process. If a module is conditionally skipped (split-mode
// filter, IfDeployment), its option chain never executes the
// declaration invokes, so the store simply doesn't see them.
type manifestStore struct {
	mu sync.Mutex

	envs        []manifest.EnvVar
	services    []manifest.ServiceNeed
	volumes     []manifest.Volume
	tasks       []manifest.StartupTask
	envProvs    []manifest.EnvProvider
	svcProvs    []manifest.ServiceDependencyProvider
	volProvs    []manifest.VolumeProvider
}

// DeclareEnv records one env var the app reads. Safe to call from any
// fx.Invoke — typically from a module-level nexus.DeclareEnv option,
// which expands to an invoke that calls this. Empty Name is silently
// dropped to keep callers from having to guard zero values when they
// build env lists from a slice.
func (a *App) DeclareEnv(e manifest.EnvVar) {
	if e.Name == "" {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.envs = append(a.manifest.envs, e)
	a.manifest.mu.Unlock()
}

// DeclareEnvProvider records a provider whose NexusEnv() is called at
// manifest assembly time. Use when a module's env list is
// data-driven (e.g. one EnvVar per registered DB connection); use
// DeclareEnv directly when the list is static.
func (a *App) DeclareEnvProvider(p manifest.EnvProvider) {
	if p == nil {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.envProvs = append(a.manifest.envProvs, p)
	a.manifest.mu.Unlock()
}

// DeclareService records a backing-service dependency (Postgres,
// Redis, RabbitMQ, etc.) the orchestration platform should provision
// and bind. The ExposeAs map drives env-var fill-in: when the
// platform binds the sidecar, it sets each named env var to the
// corresponding field of the resolved sidecar.
func (a *App) DeclareService(s manifest.ServiceNeed) {
	if s.Name == "" {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.services = append(a.manifest.services, s)
	a.manifest.mu.Unlock()
}

// DeclareServiceProvider is the data-driven counterpart to
// DeclareService.
func (a *App) DeclareServiceProvider(p manifest.ServiceDependencyProvider) {
	if p == nil {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.svcProvs = append(a.manifest.svcProvs, p)
	a.manifest.mu.Unlock()
}

// UseVolume records a writable path that must persist across
// restarts. The orchestration platform mounts a persistent volume at
// each declared path. Set Shared=true when the path must be visible
// to every replica (e.g. uploads dir read by all instances) — single-
// replica apps can leave it false.
func (a *App) UseVolume(v manifest.Volume) {
	if v.Path == "" {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.volumes = append(a.manifest.volumes, v)
	a.manifest.mu.Unlock()
}

// DeclareVolumeProvider is the data-driven counterpart to UseVolume.
func (a *App) DeclareVolumeProvider(p manifest.VolumeProvider) {
	if p == nil {
		return
	}
	a.manifest.mu.Lock()
	a.manifest.volProvs = append(a.manifest.volProvs, p)
	a.manifest.mu.Unlock()
}

// AddStartupTask registers a one-shot task that runs before listeners
// bind. Migrations and other pre-start side-effecting work belong
// here. The Run function is opaque to print mode (manifest only
// surfaces Name + Description + Phase), so a print-mode invocation
// never actually executes Run — it just lists the task so the
// orchestration platform knows one is expected.
//
// The Run function IS executed in normal boot mode, sequenced by
// runStartupTasks at the head of the lifecycle OnStart hook. Failure
// halts boot with the task name surfaced in the error.
func (a *App) AddStartupTask(t manifest.StartupTask) {
	if t.Name == "" {
		return
	}
	if t.Phase == "" {
		t.Phase = "pre-start"
	}
	a.manifest.mu.Lock()
	a.manifest.tasks = append(a.manifest.tasks, t)
	a.manifest.mu.Unlock()
}

// runStartupTasks fires every registered StartupTask whose Phase is
// "pre-start" (the only phase today; "post-start" / "pre-stop" are
// reserved for forward compat). Tasks run sequentially in registration
// order — concurrency would let one migration race another's schema
// changes, which is exactly the bug pre-start is meant to prevent.
//
// On the first error, returns immediately wrapped with the task name
// so the operator sees `nexus: startup task "migrate": <reason>`
// rather than a bare error from N levels deep. Subsequent tasks are
// skipped — once a migration fails, running the next one just
// compounds inconsistency.
//
// Tasks with a nil Run are skipped silently. They're still legal in
// the manifest (the orchestration platform may want to know "this app
// expects migrations to be run externally" without nexus actually
// running them), so a nil Run is a deliberate signal, not a bug.
func (a *App) runStartupTasks(ctx context.Context) error {
	a.manifest.mu.Lock()
	tasks := append([]manifest.StartupTask(nil), a.manifest.tasks...)
	a.manifest.mu.Unlock()
	for _, t := range tasks {
		if t.Phase != "" && t.Phase != "pre-start" {
			continue
		}
		if t.Run == nil {
			continue
		}
		if err := t.Run(); err != nil {
			return fmt.Errorf("nexus: startup task %q: %w", t.Name, err)
		}
		_ = ctx // reserved for future cancellation propagation
	}
	return nil
}

// manifestInputs gathers everything print mode needs into the shape
// manifest.Build consumes. Read-side: takes the lock briefly to copy
// slice headers, then reads registry snapshots without the lock. The
// store's slices aren't mutated after fx graph construction so this
// is safe; the lock only guards concurrent declaration writes.
func (a *App) manifestInputs() manifest.Inputs {
	a.manifest.mu.Lock()
	envs := append([]manifest.EnvVar(nil), a.manifest.envs...)
	services := append([]manifest.ServiceNeed(nil), a.manifest.services...)
	volumes := append([]manifest.Volume(nil), a.manifest.volumes...)
	tasks := append([]manifest.StartupTask(nil), a.manifest.tasks...)
	envProvs := append([]manifest.EnvProvider(nil), a.manifest.envProvs...)
	svcProvs := append([]manifest.ServiceDependencyProvider(nil), a.manifest.svcProvs...)
	volProvs := append([]manifest.VolumeProvider(nil), a.manifest.volProvs...)
	a.manifest.mu.Unlock()

	in := manifest.Inputs{
		Name:             a.dashboardName,
		Version:          a.version,
		Deployment:       a.deployment,
		Ports:            collectPorts(a.listeners),
		EnvProviders:     envProvs,
		ServiceProviders: svcProvs,
		VolumeProviders:  volProvs,
		StartupTasks:     tasks,
		DirectEnv:        envs,
		DirectServices:   services,
		DirectVolumes:    volumes,
	}

	// Auto-derive ServiceNeeds from registered NexusResources whose
	// Kind maps to a known sidecar (database / cache / queue). Lets
	// apps that already use the resource pattern (for the dashboard's
	// health pill) automatically appear in manifest.services[] without
	// also calling DeclareService — closes the gap where an app's
	// RabbitMQ wrapper registers as a "queue" resource but the
	// orchestrator can't see it as a provisionable dependency.
	//
	// Explicit DeclareService entries always win on Name conflict.
	// ExposeAs is left empty (the framework can't infer which env
	// vars the user's wrapper reads); operators wire manually OR the
	// user adds an explicit ServiceNeed with ExposeAs filled in.
	in.DirectServices = appendDerivedServicesFromResources(in.DirectServices, a.registry.Resources())

	// Registry-derived sections. The dashboard's existing endpoints
	// expose richer views; here we project just what an external
	// deployer needs to route traffic and understand the topology.
	for _, w := range a.registry.Workers() {
		in.Workers = append(in.Workers, manifest.WorkerSummary{
			Name:        w.Name,
			Description: w.Description,
		})
	}
	in.Crons = collectCrons(a)

	// Endpoint walk: emit BOTH the v0 EndpointSummary list (back-compat
	// for consumers already parsing it) AND the v1 Routes + Modules
	// shapes (richer kind taxonomy + module/deployment grouping the
	// orchestrator dashboard reads). Single pass — endpoint walk is
	// already O(n), no reason to do it twice.
	endpoints := a.registry.Endpoints()
	moduleAcc := map[string]*manifest.Module{}
	for _, e := range endpoints {
		in.Endpoints = append(in.Endpoints, manifest.EndpointSummary{
			Service:   e.Service,
			Transport: string(e.Transport),
			Method:    e.Method,
			Path:      e.Path,
		})
		r, ok := routeFromEndpoint(e)
		if !ok {
			continue
		}
		in.Routes = append(in.Routes, r)
		if e.Module == "" {
			continue // top-level endpoint, not module-owned
		}
		mod, exists := moduleAcc[e.Module]
		if !exists {
			mod = &manifest.Module{Name: e.Module, Deployment: e.Deployment}
			moduleAcc[e.Module] = mod
		}
		mod.Routes = append(mod.Routes, r.ID)
	}
	if len(moduleAcc) > 0 {
		in.Modules = make([]manifest.Module, 0, len(moduleAcc))
		for _, m := range moduleAcc {
			in.Modules = append(in.Modules, *m)
		}
	}
	// Auth + TenantScoped on Route, and Crons/Entities references on
	// Module, are intentionally left empty here. The registry doesn't
	// track auth requirements or tenant scope per endpoint today, and
	// crons don't carry a Module link. Each is a small extension to
	// the registry's Endpoint / Snapshot types — defer until at least
	// one consumer (orchestrator dashboard) actually needs the field.
	return in
}

// routeFromEndpoint translates a registry endpoint into the v1 Route
// shape, deriving Kind from Transport+Method and synthesizing a stable
// ID. Returns ok=false when the endpoint can't be classified — today
// only happens for unknown future Transport values, so the caller
// silently skips and the endpoint still appears in the v0 Endpoints
// list.
//
// ID format is intentionally human-readable rather than opaque hash:
// it shows up in Module.Routes references and dashboard URLs, so
// "users.rest.GET./users/:id" is more useful than "r-3a7b1c". Stable
// across rebuilds because it's pure function of the endpoint's
// declaration.
func routeFromEndpoint(e registry.Endpoint) (manifest.Route, bool) {
	mod := e.Module
	if mod == "" {
		// Use service name as a fallback prefix for top-level routes
		// so their IDs don't collide across services in the rare case
		// of duplicate METHOD+path under different services.
		mod = e.Service
	}
	r := manifest.Route{
		Module:     e.Module,
		Deployment: e.Deployment,
	}
	switch e.Transport {
	case registry.REST:
		r.Kind = "rest"
		r.Method = e.Method
		r.Path = e.Path
		r.ID = mod + ".rest." + e.Method + "." + e.Path
	case registry.WebSocket:
		r.Kind = "ws"
		r.Path = e.Path
		r.ID = mod + ".ws." + e.Path
	case registry.GraphQL:
		// e.Method is "query"|"mutation"|"subscription"; e.Name is the
		// operation name (e.g. "listUsers"). Keep them split on Route
		// so consumers can filter by operation type without parsing.
		r.Kind = "graphql." + e.Method
		r.Operation = e.Name
		r.ID = mod + ".gql." + e.Method + "." + e.Name
	default:
		return manifest.Route{}, false
	}
	return r, true
}

// collectPorts maps the configured listener set into manifest ports.
// Single-listener back-compat mode (empty listeners map) yields a
// nil slice — the orchestration platform falls back to the deploy
// config's declared port. Random-port listeners (`:0`) are filtered
// out: a port that won't be the same across restarts isn't useful in
// a manifest.
func collectPorts(ls map[string]Listener) []manifest.Port {
	if len(ls) == 0 {
		return nil
	}
	out := make([]manifest.Port, 0, len(ls))
	for name, l := range ls {
		port := numericPort(l.Addr)
		if port == 0 {
			continue
		}
		out = append(out, manifest.Port{
			Name:  name,
			Port:  port,
			Scope: l.Scope.String(),
		})
	}
	return out
}

// numericPort extracts the numeric port from a listener Addr like
// "127.0.0.1:8080" or ":9090". Returns 0 for "" / ":0" / parse
// failures so collectPorts can drop them.
func numericPort(addr string) int {
	if addr == "" {
		return 0
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n == 0 {
		return 0
	}
	return n
}

// collectCrons projects the scheduler's snapshot into the manifest's
// minimal CronSummary shape. The dashboard's /__nexus/crons endpoint
// returns the rich version with history; manifest just wants name +
// schedule so the deployer knows what to surface to operators.
func collectCrons(a *App) []manifest.CronSummary {
	if a.cronSched == nil {
		return nil
	}
	snaps := a.cronSched.Snapshots()
	if len(snaps) == 0 {
		return nil
	}
	out := make([]manifest.CronSummary, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, manifest.CronSummary{
			Name:     s.Name,
			Schedule: s.Schedule,
		})
	}
	return out
}

// appendDerivedServicesFromResources synthesizes manifest.ServiceNeed
// entries from registered NexusResources so apps that already use the
// resource pattern (for the dashboard's health pill) don't have to
// also call DeclareService for the orchestrator to see the dependency.
//
// Mapping rules:
//   - resource.KindDatabase  → ServiceNeed{Kind: details["driver"] | details["engine"] | "postgres"}
//   - resource.KindCache     → ServiceNeed{Kind: details["engine"]                    | "redis"}
//   - resource.KindQueue     → ServiceNeed{Kind: details["broker"]                    | "rabbitmq"}
//   - resource.KindOther     → skipped (no provisioning policy)
//
// Conflict policy: if existing already contains a ServiceNeed with
// the same Name (typically because the user explicitly declared it),
// the existing entry wins and the derivation is skipped. This makes
// auto-derivation purely additive — never overrides operator intent.
//
// ExposeAs is intentionally left empty: the framework can't know
// which env vars the user's wrapper reads. Operators wire env vars
// manually after the orchestrator binds the sidecar, OR the user
// upgrades the wrapper to declare ExposeAs explicitly.
func appendDerivedServicesFromResources(existing []manifest.ServiceNeed, resources []registry.ResourceSnapshot) []manifest.ServiceNeed {
	if len(resources) == 0 {
		return existing
	}
	known := make(map[string]struct{}, len(existing))
	for _, s := range existing {
		known[s.Name] = struct{}{}
	}
	for _, r := range resources {
		if _, dup := known[r.Name]; dup {
			continue
		}
		kind := serviceKindFromResource(r)
		if kind == "" {
			continue // unknown resource kind — no provisioning policy
		}
		existing = append(existing, manifest.ServiceNeed{
			Name: r.Name,
			Kind: kind,
		})
		known[r.Name] = struct{}{}
	}
	return existing
}

// serviceKindFromResource picks the manifest service Kind string for a
// resource snapshot, preferring details-supplied technology over the
// kind-default. Returns "" for resource.KindOther (no sensible default).
func serviceKindFromResource(r registry.ResourceSnapshot) string {
	hint := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := r.Details[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch r.Kind {
	case resource.KindDatabase:
		if k := hint("driver", "engine", "kind"); k != "" {
			return k
		}
		return "postgres"
	case resource.KindCache:
		if k := hint("engine", "driver", "kind"); k != "" {
			return k
		}
		return "redis"
	case resource.KindQueue:
		if k := hint("broker", "engine", "driver", "kind"); k != "" {
			return k
		}
		return "rabbitmq"
	}
	return ""
}

// ── Option helpers (module-level declarations) ─────────────────────
//
// These wrap fx.Invoke(func(*App) { app.DeclareXxx(...) }) so a
// module declares at graph-construction time, not at lifecycle
// start — meaning print mode sees the declarations even though
// constructors aren't fired and OnStart never runs.
//
// Pattern matches the existing nexus.Provide / nexus.Invoke option
// shape: each returns an Option whose nexusOption() yields an
// fx.Option fx can wire.

// DeclareEnv produces an Option that registers one EnvVar on the
// app at graph construction. Multiple calls compose:
//
//	nexus.Module("cache",
//	    nexus.DeclareEnv(manifest.EnvVar{Name: "REDIS_HOST", Required: true, BoundTo: "redis.host"}),
//	    nexus.DeclareEnv(manifest.EnvVar{Name: "REDIS_PORT", Required: true, BoundTo: "redis.port"}),
//	    nexus.Provide(NewManager),
//	)
func DeclareEnv(e manifest.EnvVar) Option {
	return Invoke(func(a *App) { a.DeclareEnv(e) })
}

// DeclareEnvList is the bulk variant of DeclareEnv. Used to splice in
// a slice an upstream package exposes (e.g. cache.ManifestEnv()):
//
//	nexus.Run(cfg,
//	    cache.Module,
//	    nexus.DeclareEnvList(cache.ManifestEnv()),
//	    ...
//	)
//
// Lets a leaf package describe its env surface as static data without
// importing nexus (which would cycle). The app composes the
// declaration at boot.
func DeclareEnvList(es []manifest.EnvVar) Option {
	if len(es) == 0 {
		return Options() // no-op
	}
	// Capture a copy so a caller mutating the slice afterwards
	// doesn't change what gets registered.
	cp := append([]manifest.EnvVar(nil), es...)
	return Invoke(func(a *App) {
		for _, e := range cp {
			a.DeclareEnv(e)
		}
	})
}

// DeclareService produces an Option that registers one ServiceNeed.
func DeclareService(s manifest.ServiceNeed) Option {
	return Invoke(func(a *App) { a.DeclareService(s) })
}

// UseVolume produces an Option that registers one Volume.
func UseVolume(v manifest.Volume) Option {
	return Invoke(func(a *App) { a.UseVolume(v) })
}

// AddStartupTask produces an Option that registers a startup task.
// The task's Run is preserved through to integration step 3 where
// registerLifecycle invokes it before binding listeners.
func AddStartupTask(t manifest.StartupTask) Option {
	return Invoke(func(a *App) { a.AddStartupTask(t) })
}

// manifestAutoRegisterInvoke is the manifest-side counterpart to
// resourceAutoRegisterInvoke. When nexus.Provide is given a
// constructor whose return type implements one of the manifest
// provider interfaces, we synthesize an fx.Invoke(func(*App, T))
// that registers the constructed value with the right declarator.
//
// Result: developer writes nexus.Provide(NewRabbitMQ) — no
// DeclareEnv/DeclareService calls in main.go — and the returned
// *RabbitMQ is auto-walked at print-mode boot, populating the
// manifest. Same shape as how NexusResources() flows today.
//
// Returns nil when the constructor's return type doesn't implement
// any manifest provider interface, so plain types pay nothing.
func manifestAutoRegisterInvoke(fn any) fx.Option {
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func || rt.NumOut() == 0 {
		return nil
	}
	outType := rt.Out(0)
	envIface := reflect.TypeOf((*manifest.EnvProvider)(nil)).Elem()
	svcIface := reflect.TypeOf((*manifest.ServiceDependencyProvider)(nil)).Elem()
	volIface := reflect.TypeOf((*manifest.VolumeProvider)(nil)).Elem()
	if !outType.Implements(envIface) && !outType.Implements(svcIface) && !outType.Implements(volIface) {
		return nil
	}

	invokeType := reflect.FuncOf(
		[]reflect.Type{reflect.TypeOf((*App)(nil)), outType},
		nil, false,
	)
	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		inst := args[1].Interface()
		if p, ok := inst.(manifest.EnvProvider); ok {
			app.DeclareEnvProvider(p)
		}
		if p, ok := inst.(manifest.ServiceDependencyProvider); ok {
			app.DeclareServiceProvider(p)
		}
		if p, ok := inst.(manifest.VolumeProvider); ok {
			app.DeclareVolumeProvider(p)
		}
		return nil
	})
	return fx.Invoke(invokeFn.Interface())
}

// ── Type-assert that registry shapes match what we expect ──────────
//
// Compile-time guard: if registry.Worker / registry.Endpoint ever
// changes Name/Description/etc., this var declaration fails to build
// and we know to update collectors above. Cheaper than an integration
// test for catching field renames.
var _ = registry.Worker{Name: "", Description: ""}

// Compile-time guard: *App MUST satisfy manifest.Registrar. Carved
// here so a future signature drift on the interface (or a method
// rename on *App) blows up the build instead of producing a wrong-
// type panic at fx.Run time.
var _ manifest.Registrar = (*App)(nil)