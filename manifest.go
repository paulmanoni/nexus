package nexus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"sync"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/manifest"
	"github.com/paulmanoni/nexus/v2/registry"
	"github.com/paulmanoni/nexus/v2/resource"
)

// manifestStore holds every declaration registered against an *App by
// the option helpers below (DeclareEnv, DeclareService, DeclareVolume,
// AddStartupTask) plus the corresponding *Provider variants. All
// access goes through manifestMu so concurrent invokes don't corrupt
// the slices — fx invokes are sequential today but it's cheap
// insurance and keeps the public methods safe to call from anywhere.
//
// The store is intentionally additive-only. There's no Unregister:
// once a module has declared its env vars / service needs, those
// declarations are part of the app's identity for the lifetime of
// the process. If a module is conditionally skipped (a split-mode
// filter), its option chain never executes the
// declaration invokes, so the store simply doesn't see them.
type manifestStore struct {
	mu sync.Mutex

	envs     []manifest.EnvVar
	services []manifest.ServiceNeed
	volumes  []manifest.Volume
	tasks    []manifest.StartupTask
	envProvs []manifest.EnvProvider
	svcProvs []manifest.ServiceDependencyProvider
	volProvs []manifest.VolumeProvider

	// inputs surface (v0.39+) — additive declarations the orchestration
	// platform consumes. Empty for apps that haven't adopted the cloud
	// contract yet; pass through unused.
	environments []manifest.Environment
	secrets      []manifest.Secret
	files        []manifest.File
	hooks        *manifest.Hooks
	overrides    map[string]manifest.Override

	// Plugin-driven blocks read from nexus.toml by
	// LoadDeployManifest. Each is opaque to the framework — the
	// corresponding plugin reads it back via app.EffectiveManifest()
	// at boot.
	tls    *manifest.TLSBlock
	cors   *manifest.CORSBlock
	errors *manifest.ErrorsBlock

	// effective is the merged manifest for the active environment,
	// populated once at boot by ResolveEffective. nil before that
	// call (e.g. during print mode); /__nexus/manifest then falls
	// back to building from the declared base.
	effective *manifest.Manifest
}

// declareAppend appends v to one of the manifest store's slices under
// its lock — the shared body of the append-style Declare* methods.
func declareAppend[T any](a *App, dst *[]T, v T) {
	a.manifest.mu.Lock()
	*dst = append(*dst, v)
	a.manifest.mu.Unlock()
}

// declareReplace sets one of the manifest store's block pointers under
// its lock (last call wins) — the shared body of the replace-style
// Declare* methods.
func declareReplace[T any](a *App, dst **T, v T) {
	a.manifest.mu.Lock()
	cp := v
	*dst = &cp
	a.manifest.mu.Unlock()
}

// DeclareEnv records one env var the app reads. Safe to call from any
// di.Invoke — typically from a module-level nexus.DeclareEnv option,
// which expands to an invoke that calls this. Empty Name is silently
// dropped to keep callers from having to guard zero values when they
// build env lists from a slice.
func (a *App) DeclareEnv(e manifest.EnvVar) {
	if e.Name == "" {
		return
	}
	declareAppend(a, &a.manifest.envs, e)
}

// DeclareEnvProvider records a provider whose NexusEnv() is called at
// manifest assembly time. Use when a module's env list is
// data-driven (e.g. one EnvVar per registered DB connection); use
// DeclareEnv directly when the list is static.
func (a *App) DeclareEnvProvider(p manifest.EnvProvider) {
	if p == nil {
		return
	}
	declareAppend(a, &a.manifest.envProvs, p)
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
	declareAppend(a, &a.manifest.services, s)
}

// DeclareServiceProvider is the data-driven counterpart to
// DeclareService.
func (a *App) DeclareServiceProvider(p manifest.ServiceDependencyProvider) {
	if p == nil {
		return
	}
	declareAppend(a, &a.manifest.svcProvs, p)
}

// DeclareVolume records a writable path that must persist across
// restarts. The orchestration platform mounts a persistent volume at
// each declared path. Set Shared=true when the path must be visible
// to every replica (e.g. uploads dir read by all instances) — single-
// replica apps can leave it false.
func (a *App) DeclareVolume(v manifest.Volume) {
	if v.Path == "" {
		return
	}
	declareAppend(a, &a.manifest.volumes, v)
}

// DeclareVolumeProvider is the data-driven counterpart to DeclareVolume.
func (a *App) DeclareVolumeProvider(p manifest.VolumeProvider) {
	if p == nil {
		return
	}
	declareAppend(a, &a.manifest.volProvs, p)
}

// DeclareSecret records one sensitive input the app reads. Distinct
// from DeclareEnv so the platform's secret store can manage it
// separately (encrypted at rest, redacted in UI, rotation reminders).
// Empty Name is silently dropped.
func (a *App) DeclareSecret(s manifest.Secret) {
	if s.Name == "" {
		return
	}
	declareAppend(a, &a.manifest.secrets, s)
}

// DeclareFile records a mounted-blob input — TLS bundle, JSON config
// override, etc. The platform writes the bytes to Path at deploy time.
// Empty Name or Path is silently dropped.
func (a *App) DeclareFile(f manifest.File) {
	if f.Name == "" || f.Path == "" {
		return
	}
	declareAppend(a, &a.manifest.files, f)
}

// DeclareEnvironment records a deploy target ("production",
// "staging", "preview", ...). The orchestration platform consults
// the list to know which environments the binary is built for; the
// active one (Config.Environment / NEXUS_ENVIRONMENT) is matched
// against this set at boot.
func (a *App) DeclareEnvironment(e manifest.Environment) {
	if e.Name == "" {
		return
	}
	a.manifest.mu.Lock()
	// Idempotent: skip if a same-named environment is already declared.
	// Lets multiple modules each call DeclareEnvironment("production")
	// without producing duplicate manifest entries.
	for _, existing := range a.manifest.environments {
		if existing.Name == e.Name {
			a.manifest.mu.Unlock()
			return
		}
	}
	a.manifest.environments = append(a.manifest.environments, e)
	a.manifest.mu.Unlock()
}

// DeclareHooks sets the platform-orchestrated build/predeploy/
// postdeploy commands. Idempotent within a single boot: subsequent
// calls fully replace the previous Hooks block (rather than
// accumulating) — typical use is one call from the top-level main()
// with the full set.
func (a *App) DeclareHooks(h manifest.Hooks) {
	declareReplace(a, &a.manifest.hooks, h)
}

// DeclareTLS sets the public-internet TLS configuration block read
// by the extension/tls plugin at boot. Idempotent within a single
// boot: subsequent calls fully replace the previous block.
func (a *App) DeclareTLS(t manifest.TLSBlock) {
	declareReplace(a, &a.manifest.tls, t)
}

// DeclareCORS sets the cross-origin policy block read by the
// extension/cors plugin at boot. Idempotent — last call wins.
func (a *App) DeclareCORS(c manifest.CORSBlock) {
	declareReplace(a, &a.manifest.cors, c)
}

// DeclareErrors sets the error-capture configuration block read by
// the extension/errors plugin at boot. Idempotent — last call wins.
func (a *App) DeclareErrors(e manifest.ErrorsBlock) {
	declareReplace(a, &a.manifest.errors, e)
}

// DeclareOverride registers a per-environment Override against the
// declared inputs. env must match a previously-declared Environment;
// validation happens at merge time via manifest.MergeOverrides, not
// here, so the declaration order doesn't matter.
//
// Calling DeclareOverride twice for the same env REPLACES the
// previous diff — there's no merging at registration time. This
// matches operator intent: one module owns the prod override for a
// given env, not a chain of modules each contributing slices.
func (a *App) DeclareOverride(env string, ov manifest.Override) {
	if env == "" {
		return
	}
	a.manifest.mu.Lock()
	if a.manifest.overrides == nil {
		a.manifest.overrides = make(map[string]manifest.Override)
	}
	a.manifest.overrides[env] = ov
	a.manifest.mu.Unlock()
}

// LoadDeployManifest reads nexus.toml at path, parses its
// inputs surface (environments / secrets / files / hooks /
// environment_overrides), and registers each entry through the
// existing Declare* methods. Idempotent: re-declaring an environment
// with the same name is a no-op, secrets / files dedup by name, and
// DeclareOverride replaces any prior diff for the same environment.
//
// Intended call sites:
//
//   - Directly in main() before nexus.Run, when the operator wants
//     YAML to be the source of truth for the inputs surface.
//   - From a nexus.Invoke(...) for apps that want the inputs to
//     participate in fx ordering.
//   - From a codegen'd init() emitted by `nexus build` (future
//     extension — the codegen path that currently bakes deployment
//     defaults will also bake DeclareEnvironment / DeclareSecret /
//     DeclareOverride calls so the runtime stays file-IO-free).
//
// YAML parse errors return immediately. Schema validation (duplicate
// names, malformed validation rules) is NOT performed here — call
// manifest.Lint() on the result manifest to surface those issues.
// Boot validation against actual env values still runs at di.Start
// via resolveEffectiveManifest.
//
// Missing file is treated as an error so a typo in the path doesn't
// silently produce an empty inputs surface. To make the call optional,
// stat the file first or wrap in a guard.
func (a *App) LoadDeployManifest(path string) error {
	loaded, err := manifest.LoadInputsTOMLFile(path)
	if err != nil {
		return err
	}
	for _, e := range loaded.Environments {
		a.DeclareEnvironment(e)
	}
	for _, s := range loaded.Secrets {
		a.DeclareSecret(s)
	}
	for _, f := range loaded.Files {
		a.DeclareFile(f)
	}
	if loaded.Hooks != nil {
		a.DeclareHooks(*loaded.Hooks)
	}
	// Plugin-driven blocks. Each one corresponds to an extension/*
	// plugin that reads its config from the effective manifest at
	// boot. Without these calls the YAML block parses successfully
	// but never propagates into app.EffectiveManifest() — silent
	// drop, plugin then fails with "Config.X is required".
	if loaded.TLS != nil {
		a.DeclareTLS(*loaded.TLS)
	}
	if loaded.CORS != nil {
		a.DeclareCORS(*loaded.CORS)
	}
	if loaded.Errors != nil {
		a.DeclareErrors(*loaded.Errors)
	}
	for env, ov := range loaded.Overrides {
		a.DeclareOverride(env, ov)
	}
	return nil
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
		if err := t.Run(ctx); err != nil {
			return fmt.Errorf("nexus: startup task %q: %w", t.Name, err)
		}
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
	environments := append([]manifest.Environment(nil), a.manifest.environments...)
	secrets := append([]manifest.Secret(nil), a.manifest.secrets...)
	files := append([]manifest.File(nil), a.manifest.files...)
	var hooks *manifest.Hooks
	if a.manifest.hooks != nil {
		h := *a.manifest.hooks
		hooks = &h
	}
	var overrides map[string]manifest.Override
	if len(a.manifest.overrides) > 0 {
		overrides = make(map[string]manifest.Override, len(a.manifest.overrides))
		for k, v := range a.manifest.overrides {
			overrides[k] = v
		}
	}
	// Plugin-driven blocks — copy by value so the Inputs snapshot
	// doesn't alias the store's pointer. Inputs.Build will then
	// deep-copy the slice fields when it materializes the Manifest.
	var tls *manifest.TLSBlock
	if a.manifest.tls != nil {
		t := *a.manifest.tls
		tls = &t
	}
	var cors *manifest.CORSBlock
	if a.manifest.cors != nil {
		c := *a.manifest.cors
		cors = &c
	}
	var errors *manifest.ErrorsBlock
	if a.manifest.errors != nil {
		e := *a.manifest.errors
		errors = &e
	}
	a.manifest.mu.Unlock()

	in := manifest.Inputs{
		Name:             a.dashboardName,
		Version:          a.version,
		Ports:            collectPorts(a.listeners),
		EnvProviders:     envProvs,
		ServiceProviders: svcProvs,
		VolumeProviders:  volProvs,
		StartupTasks:     tasks,
		DirectEnv:        envs,
		DirectServices:   services,
		DirectVolumes:    volumes,
		Environments:     environments,
		DirectSecrets:    secrets,
		DirectFiles:      files,
		Hooks:            hooks,
		Overrides:        overrides,
		TLS:              tls,
		CORS:             cors,
		Errors:           errors,
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
func collectPorts(ls map[string]config.Listener) []manifest.Port {
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
// These wrap di.Invoke(func(*App) { app.DeclareXxx(...) }) so a
// module declares at graph-construction time, not at lifecycle
// start — meaning print mode sees the declarations even though
// constructors aren't fired and OnStart never runs.
//
// Pattern matches the existing nexus.Provide / nexus.Invoke option
// shape: each returns an Option whose nexusOption() yields an
// di.Option fx can wire.

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

// DeclareVolume produces an Option that registers one Volume.
func DeclareVolume(v manifest.Volume) Option {
	return Invoke(func(a *App) { a.DeclareVolume(v) })
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
// provider interfaces, we synthesize an di.Invoke(func(*App, T))
// that registers the constructed value with the right declarator.
//
// Result: developer writes nexus.Provide(NewRabbitMQ) — no
// DeclareEnv/DeclareService calls in main.go — and the returned
// *RabbitMQ is auto-walked at print-mode boot, populating the
// manifest. Same shape as how NexusResources() flows today.
//
// Returns nil when the constructor's return type doesn't implement
// any manifest provider interface, so plain types pay nothing.
func manifestAutoRegisterInvoke(fn any) di.Option {
	return autoRegisterInvoke(fn,
		[]reflect.Type{
			reflect.TypeFor[manifest.EnvProvider](),
			reflect.TypeFor[manifest.ServiceDependencyProvider](),
			reflect.TypeFor[manifest.VolumeProvider](),
		},
		func(app *App, inst any) {
			if p, ok := inst.(manifest.EnvProvider); ok {
				app.DeclareEnvProvider(p)
			}
			if p, ok := inst.(manifest.ServiceDependencyProvider); ok {
				app.DeclareServiceProvider(p)
			}
			if p, ok := inst.(manifest.VolumeProvider); ok {
				app.DeclareVolumeProvider(p)
			}
		})
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
// type panic at di.Run time.
var _ manifest.Registrar = (*App)(nil)

// DefaultManifestPath is the file the framework auto-loads from the
// current working directory at boot. Operators who need a different
// path call `app.LoadDeployManifest("alt-path.toml")` from their own
// nexus.Invoke — that runs AFTER auto-load and the framework's
// declarations are idempotent (last writer wins on each block) so
// explicit overrides land cleanly.
const DefaultManifestPath = "nexus.toml"

// autoManifestEnvSkip lets operators opt out of auto-loading when
// they need to run multiple manifest files via explicit
// LoadDeployManifest calls without the auto-loader stepping on the
// first one. Set NEXUS_SKIP_MANIFEST_AUTOLOAD=1 to disable.
const autoManifestEnvSkip = "NEXUS_SKIP_MANIFEST_AUTOLOAD"

// autoManifestOptions returns the di.Invoke that auto-loads
// nexus.toml from the current working directory if it exists.
// Lets plugins (extension/tls, /cors, /errors, ...) read
// their per-environment config via app.EffectiveManifest() without
// the operator having to write the boilerplate:
//
//	nexus.Invoke(func(app *nexus.App) {
//	    if err := app.LoadDeployManifest("nexus.toml"); err != nil {
//	        panic(err)
//	    }
//	})
//
// Behavior:
//
//   - File missing  → silent skip. Apps without a manifest boot
//     normally; the operator hasn't adopted the
//     cloud-shaped input surface yet.
//   - File present  → parse + Declare each block. Existing Declare*
//     calls from user code keep their semantics
//     (last writer wins per block).
//   - Parse error   → panic with a clear message. A malformed
//     manifest is an operator bug; failing fast at
//     boot is the right behavior. Same shape as
//     LoadDeployManifest's documented contract.
//   - Other I/O err → logged + skipped. Permission glitches in a
//     dev environment shouldn't crash the app.
//
// Disable with NEXUS_SKIP_MANIFEST_AUTOLOAD=1 for the rare case
// where two manifest files must be loaded in a specific order.
func autoManifestOptions() di.Option {
	if os.Getenv(autoManifestEnvSkip) == "1" {
		return di.Options()
	}
	return di.Invoke(func(app *App) {
		// Same resolution as the runtime-config loader (NEXUS_CONFIG,
		// then cwd, then next to the executable) — the manifest blocks
		// live in the same nexus.toml, so loading runtime config from
		// one file and manifest blocks from another would split-brain
		// a NEXUS_CONFIG deployment.
		path := resolveConfigPath()
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				// Permission denied, I/O error — log it but keep
				// booting; the operator can recover by fixing the
				// file ACL and restarting.
				log.Printf("nexus: manifest auto-load: stat %s: %v", path, err)
			}
			return
		}
		if err := app.LoadDeployManifest(path); err != nil {
			// Parse error / schema mismatch / etc. Operator bug —
			// surface loud rather than silently dropping the
			// manifest and watching plugins fail downstream with
			// confusing "Config.X is required" messages.
			panic("nexus: manifest auto-load: " + err.Error())
		}
	})
}

// printManifestEnv is consulted by Run to decide whether to short-
// circuit into print mode. Mirrors manifest.EnvVarPrintAndExit so
// options.go doesn't need an import that crosses out and back into
// the manifest package.
const printManifestEnv = manifest.EnvVarPrintAndExit

// printManifestAndExitIfRequested is the build-time/upload-time
// extraction path that replaces the rejected `/__nexus/manifest`
// HTTP endpoint.
//
// The orchestration platform invokes the built image once, with
// NEXUS_PRINT_MANIFEST=1 set:
//
//	docker run --rm -e NEXUS_PRINT_MANIFEST=1 <image>
//
// The container boots its fx graph far enough to resolve every
// module-level declaration (DeclareEnv / DeclareService / DeclareVolume /
// AddStartupTask), prints the assembled manifest as JSON to stdout,
// and exits 0 — without binding listeners, without dialing Redis or
// Postgres, without firing any startup task. The orchestration
// platform stores the captured JSON on the build row and uses it to
// plan the actual deploy (sidecars, env, volumes, migrations).
//
// Why a build-time exit and not an HTTP endpoint:
//   - Manifest is needed BEFORE the app runs to plan dependencies; a
//     runtime endpoint is too late and would create a chicken-and-egg
//     for sidecars/env that would otherwise be needed to boot.
//   - Build-time JSON is diffable in CI to catch breaking deploy-
//     config changes before merge.
//   - No public network surface to defend.
//
// Wiring (intended; this function is the seam, called from Run):
//
//	func Run(cfg Config, opts ...Option) {
//	    cfg = resolveConfig(cfg)
//	    if err := validateTopology(cfg); err != nil { panic(err) }
//
//	    if os.Getenv(manifest.EnvVarPrintAndExit) == "1" {
//	        printManifestAndExitIfRequested(cfg, opts) // exits 0 or non-0
//	    }
//
//	    // ...existing fx.New(...).Run() path unchanged...
//	}
//
// This function builds the fx graph, populates *App, prints, and
// exits — it never returns to the caller. Side-effect contract: any
// EnvProvider / ServiceDependencyProvider / VolumeProvider
// implementation MUST be cheap and side-effect-free, because their
// methods are called as part of manifest assembly. Constructors that
// dial external systems should not register declarations from inside
// themselves; declare at module level instead (see "Integration"
// step 4 in manifest/manifest.go).
func printManifestAndExitIfRequested(cfg config.Runtime, opts []Option) {
	// Print mode must produce JSON-and-only-JSON on stdout — anything
	// else breaks downstream parsers (`nexus reconcile`, `nexus build
	// --emit-manifest`, the orchestrator's extractManifest). The default
	// stdlib router emits no route-registration noise, so the old gin
	// log-silencing is gone; the opt-in gin backend self-silences via
	// release mode in ginrouter.New.

	// Build the same option chain Run uses, INCLUDING fxLateOptions
	// because autoMountGraphQL is what walks the gqlField group and
	// registers GraphQL endpoints into the registry. Without it,
	// nexus.AsQuery / nexus.AsMutation declarations stay invisible to
	// the manifest's Routes section — the user sees REST endpoints
	// only, even though their app has a fully-wired GraphQL surface.
	//
	// Mounting routes on the engine in print mode is harmless: no
	// listeners bind (registerLifecycle's OnStart never fires because
	// fx.Populate doesn't fire lifecycle hooks), so the engine is
	// just a registry-of-routes, never a server.
	all := append([]di.Option{fxEarlyOptions(cfg)}, unwrap(opts)...)
	all = append(all, unwrap(filterDeferredOptions(opts, collectDeferredOptions()))...)
	all = append(all, fxLateOptions())

	// Drop the lifecycle invoke from fxEarlyOptions if it ever grows
	// side-effecting OnStart hooks beyond listener bind (today
	// registerLifecycle's OnStart binds listeners — we'd rather not
	// run that loop at all, so we'd ideally swap fxEarlyOptions for
	// a print-only variant. Tracked as an open item; di.Populate runs
	// provides + invokes eagerly but never fires lifecycle OnStart
	// hooks (those wait for Start, which we never call here).

	var app *App
	all = append(all, di.Populate(&app))

	beginBuild()
	built := di.New(all...)
	if err := built.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: graph error:", err)
		os.Exit(2)
	}
	if app == nil {
		// New ran but Populate didn't fill *App — should not
		// happen given fxEarlyOptions provides New, but guard anyway
		// so a future refactor doesn't silently print {}.
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: *App not provided by graph")
		os.Exit(2)
	}

	// Build manifest from inputs. manifestInputs is the *App method
	// added in step 1 of the integration sequence — it gathers
	// providers, registry snapshots, and listener ports into
	// manifest.Inputs. Until that lands, this call is a stub returning
	// a near-empty manifest with just identity + ports populated from
	// what *App already exposes.
	in := app.manifestInputs()
	if err := manifest.PrintJSON(os.Stdout, manifest.Build(in)); err != nil {
		fmt.Fprintln(os.Stderr, "nexus: print-manifest: encode:", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// (manifestInputs lives on *App in manifest.go alongside the
// declaration store and option helpers.)

// resolveEffectiveManifest builds the base manifest from declared
// inputs, merges per-environment overrides for the active environment,
// runs boot-time validation, and caches the result on the App so
// /__nexus/manifest and future provenance endpoints can serve a single
// consistent view. Called once from registerLifecycle's OnStart after
// runStartupTasks completes.
//
// The function is split into stages so each step's failure mode is
// clear:
//
//	build  → merge  → validate
//	  │        │        └─ required env vars present, validation rules satisfied
//	  │        └─ override applied (or no-op if no overrides)
//	  └─ all DeclareEnv / DeclareSecret / etc. invokes have run by now
//
// On any error returns a *UserError so the boot failure surfaces with
// op / hint / cause structure rather than a flat string.
func (a *App) resolveEffectiveManifest() error {
	base := manifest.Build(a.manifestInputs())

	env := a.environment
	// If the app didn't declare the active environment, MergeOverrides
	// would reject. Inject a synthetic Environment entry so an app that
	// uses the cloud contract loosely (declares overrides for some envs
	// but not all) still boots — the merge then is a no-op deep copy.
	if env != "" && !environmentDeclared(base.Environments, env) {
		if len(base.Overrides) > 0 {
			// Overrides exist for some env: insist on the active one
			// being declared. Reject with a clear message so the
			// operator sees the typo / misconfig.
			return &UserError{
				Op:   "manifest.resolve",
				Msg:  fmt.Sprintf("active environment %q is not declared in manifest.environments", env),
				Hint: fmt.Sprintf("add it via app.DeclareEnvironment(manifest.Environment{Name: %q}) — declared envs: %v", env, manifest.AvailableEnvironments(base)),
			}
		}
		// No overrides at all → treat the active env as a no-op
		// passthrough by synthesizing a minimal Environment entry so
		// MergeOverrides accepts it.
		base.Environments = append(base.Environments, manifest.Environment{Name: env})
	}

	effective, err := manifest.MergeOverrides(base, env)
	if err != nil {
		return &UserError{
			Op:    "manifest.resolve",
			Msg:   fmt.Sprintf("override merge failed for environment %q", env),
			Cause: err,
			Hint:  "fix the override in nexus.toml (or DeclareOverride) — check manifest.Lint() for write-time validation",
		}
	}

	if err := validateEffectiveEnv(effective); err != nil {
		return err
	}
	if err := validateEffectiveSecrets(effective); err != nil {
		return err
	}

	a.manifest.mu.Lock()
	a.manifest.effective = &effective
	a.manifest.mu.Unlock()
	return nil
}

// validateEffectiveEnv checks every declared env var against the
// effective manifest's expectations: required vars must be set in the
// process env (or have a default), and any value present must satisfy
// the Validation block. Returns the FIRST error so the operator can
// fix one problem at a time (lint reports all-at-once at write time;
// boot is fail-fast).
func validateEffectiveEnv(m manifest.Manifest) error {
	for _, e := range m.Env {
		value, set := lookupEnvValue(e.Name)
		// Empty env var is treated as missing — a deliberate empty
		// string from the platform doesn't satisfy Required, and
		// `unset` and `set to ""` should fail validation the same way.
		if !set || value == "" {
			if e.Required && e.Default == "" && e.BoundTo == "" {
				return &UserError{
					Op:   "manifest.validate",
					Msg:  fmt.Sprintf("required env var %s is not set", e.Name),
					Hint: fmt.Sprintf("set %s in the platform / .env / shell, OR declare a Default", e.Name),
				}
			}
			value = e.Default
		}
		if value == "" {
			continue
		}
		if msg := checkValue(value, e.Validation); msg != "" {
			return &UserError{
				Op:   "manifest.validate",
				Msg:  fmt.Sprintf("env %s: %s", e.Name, msg),
				Hint: "loosen the validation rule, OR set a value that satisfies it",
			}
		}
	}
	return nil
}

// validateEffectiveSecrets mirrors validateEffectiveEnv for secrets.
// Secrets are conventionally surfaced as env vars at runtime (the
// platform writes them in), so the same lookupEnvValue path applies.
func validateEffectiveSecrets(m manifest.Manifest) error {
	for _, s := range m.Secrets {
		value, set := lookupEnvValue(s.Name)
		if !set || value == "" {
			if s.Required {
				return &UserError{
					Op:   "manifest.validate",
					Msg:  fmt.Sprintf("required secret %s is not set", s.Name),
					Hint: fmt.Sprintf("provide via the platform's secret store, OR set %s in the local shell for dev", s.Name),
				}
			}
			continue
		}
		if msg := checkValue(value, s.Validation); msg != "" {
			return &UserError{
				Op:  "manifest.validate",
				Msg: fmt.Sprintf("secret %s: %s", s.Name, msg),
			}
		}
	}
	return nil
}

// checkValue applies a Validation rule set to a single string value.
// Returns "" on success or a human-readable rule violation. Numeric
// Min/Max are interpreted only when the value parses as an int; non-
// numeric values just skip those rules so a free-form string isn't
// accidentally penalized.
func checkValue(value string, v *manifest.EnvValidation) string {
	if v == nil {
		return ""
	}
	if len(v.Enum) > 0 {
		ok := false
		for _, e := range v.Enum {
			if e == value {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Sprintf("value %q not in enum %v", value, v.Enum)
		}
	}
	if v.Regex != "" {
		re, err := regexp.Compile(v.Regex)
		if err == nil && !re.MatchString(value) {
			return fmt.Sprintf("value %q does not match regex %q", value, v.Regex)
		}
	}
	if v.Length != nil {
		if v.Length.Min != nil && len(value) < *v.Length.Min {
			return fmt.Sprintf("value has length %d, below length.min %d", len(value), *v.Length.Min)
		}
		if v.Length.Max != nil && len(value) > *v.Length.Max {
			return fmt.Sprintf("value has length %d, above length.max %d", len(value), *v.Length.Max)
		}
	}
	return ""
}

// lookupEnvValue is a thin wrapper around os.LookupEnv. Centralized
// so a future test seam or platform shim can intercept without
// touching every call site.
func lookupEnvValue(name string) (string, bool) {
	return os.LookupEnv(name)
}

// environmentDeclared mirrors manifest.environmentDeclared but is
// duplicated here to avoid exporting the manifest helper just for one
// internal call.
func environmentDeclared(envs []manifest.Environment, name string) bool {
	for _, e := range envs {
		if e.Name == name {
			return true
		}
	}
	return false
}

// EffectiveManifest returns the merged + validated manifest produced
// at boot. Returns nil before fx.Start completes — callers serving
// /__nexus/manifest fall back to manifest.Build(manifestInputs()) so
// print mode and pre-boot inspection still work.
func (a *App) EffectiveManifest() *manifest.Manifest {
	a.manifest.mu.Lock()
	defer a.manifest.mu.Unlock()
	if a.manifest.effective == nil {
		return nil
	}
	// Return a shallow copy so callers can't mutate the cached value.
	// Slices inside are shared — they're already-finalized read-only
	// snapshots; cloning every nested struct would be wasteful for the
	// JSON-serialize-then-discard usage at /__nexus/manifest.
	cp := *a.manifest.effective
	return &cp
}
