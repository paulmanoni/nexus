// Package manifest is the deploy-time self-description surface for a nexus
// app. It produces the JSON returned at GET /__nexus/manifest — a single
// document an external orchestration platform reads to provision the app
// without per-app glue.
//
// The contract is opt-in: an app boots fine with no manifest providers
// registered (the resulting manifest just has empty arrays for those
// sections). Adoption is incremental — modules implement the small
// interfaces below as they have something to declare.
//
// Wiring (planned, see "Integration" at the bottom of this file):
//
//  1. *nexus.App grows three registration methods:
//       app.DeclareEnv(EnvProvider)
//       app.DeclareService(ServiceDependency)
//       app.UseVolume(Volume)
//     plus a startup-task registration that collects via an fx group
//     "nexus.startup-tasks" so any module can declare one without holding
//     an *App reference.
//  2. dashboard.Mount accepts a func() Manifest and exposes
//     GET /__nexus/manifest as Handler() below.
//  3. The framework's own infra modules (cache, ratelimit, future db)
//     ship default EnvProvider implementations so apps that use them
//     get accurate env-var declarations for free.
package manifest

import (
	"encoding/json"
	"io"
	"sort"
)

// EnvVarPrintAndExit, when set to "1" in the app's environment,
// instructs nexus.Run to wire the fx graph far enough to collect
// every declared EnvVar / ServiceNeed / Volume / StartupTask, print
// the resulting Manifest as JSON to stdout, and exit 0 — without
// binding listeners or starting any background work.
//
// This is the orchestration platform's intended path for getting the
// manifest: at upload / git-sync time, the platform builds the
// project's image, then runs `docker run --rm -e NEXUS_PRINT_MANIFEST=1
// <image>` once. The container prints the manifest and exits, the
// platform stores it on the build row, and the deploy planner reads
// it offline. No HTTP exposure, no auth surface, no chicken-and-egg
// for sidecars/env that would otherwise be needed to boot.
//
// Implementations of EnvProvider / ServiceDependencyProvider /
// VolumeProvider must keep their declarations side-effect-free —
// returning static metadata, not probing Redis or opening DB
// connections — because print mode walks them before the app's
// real lifecycle starts.
const EnvVarPrintAndExit = "NEXUS_PRINT_MANIFEST"

// Manifest is the full self-description an external deployer reads.
// All slices are nil-able (omitempty) so an app that declares nothing
// still produces a valid, minimal document.
type Manifest struct {
	// Identity ─────────────────────────────────────────────────────
	Name       string `json:"name"`
	Version    string `json:"version,omitempty"`    // *App.Version()
	Deployment string `json:"deployment,omitempty"` // *App.Deployment(); "" = monolith

	// Network ──────────────────────────────────────────────────────
	Ports []Port `json:"ports,omitempty"`

	// Health probes the orchestration platform should hit. Liveness +
	// Readiness are framework-owned (always /__nexus/health and
	// /__nexus/ready); App is an optional app-level probe — the
	// orchestration platform's "container ready" gate after the
	// framework probes pass.
	Health Health `json:"health"`

	// Configuration the operator must (or may) supply. Aggregated
	// from every registered EnvProvider; deduplicated by Name with
	// last-writer-wins on description / required / secret.
	Env []EnvVar `json:"env,omitempty"`

	// Backing services to provision. Each entry names a logical sidecar
	// (a Postgres, a Redis, etc.); the orchestration platform decides
	// how to satisfy it (managed sidecar, external pool, etc.) and
	// injects the env vars listed in ExposeAs.
	Services []ServiceNeed `json:"services,omitempty"`

	// Writable paths the container needs preserved across restarts.
	Volumes []Volume `json:"volumes,omitempty"`

	// One-shot tasks run before the HTTP listener binds. Migrations,
	// schema sync, seed data. Failure halts boot.
	StartupTasks []StartupTask `json:"startupTasks,omitempty"`

	// Echoed from the existing registry, included here so a deployer
	// reads ONE document. The dashboard's other endpoints continue to
	// serve their richer per-domain views — this is the deploy-time
	// projection.
	Workers   []WorkerSummary   `json:"workers,omitempty"`
	Crons     []CronSummary     `json:"crons,omitempty"`
	Endpoints []EndpointSummary `json:"endpoints,omitempty"`
}

// Port describes one listening socket. The orchestration platform uses
// these to publish container ports + decide which to expose externally.
//
// Scope mirrors nexus's listener scope ("public" | "admin" | "internal");
// orchestration treats anything other than "public" as cluster-internal
// by default.
type Port struct {
	Name  string `json:"name"`            // e.g. "http", "admin"
	Port  int    `json:"port"`            // numeric only — Addr ":9390" → 9390
	Scope string `json:"scope,omitempty"` // matches nexus.ListenerScope strings
}

// Health is the probe map. Each path is rooted at the app's external
// URL; the orchestration platform appends them to the public Port for
// liveness / readiness HTTP probes.
type Health struct {
	Liveness  string `json:"liveness"`            // always "/__nexus/health" today
	Readiness string `json:"readiness"`           // always "/__nexus/ready"
	App       string `json:"app,omitempty"`       // optional app-level probe
}

// EnvVar is one configuration knob the app reads. Required signals
// "boot won't proceed without this"; Secret signals "store this in a
// secret manager, not the deploy YAML". BoundTo is a hint for
// orchestration: when a ServiceNeed exposes "host" via this var, the
// platform can fill it automatically without operator intervention.
type EnvVar struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Default     string `json:"default,omitempty"`
	// BoundTo is a dotted reference into a ServiceNeed entry, e.g.
	// "primary-db.host" means "fill this env var with the resolved host
	// of the ServiceNeed named primary-db". Empty when the operator
	// must supply the value directly.
	BoundTo string `json:"boundTo,omitempty"`
}

// ServiceNeed is a logical sidecar this app needs to talk to. The
// orchestration platform decides provisioning policy. Kind is
// intentionally a string (not enum) so apps can declare bespoke
// dependencies the platform may not natively support yet — operators
// still see the requirement and can wire something manually.
type ServiceNeed struct {
	Name    string `json:"name"`              // unique within the app, e.g. "primary-db"
	Kind    string `json:"kind"`              // "postgres" | "redis" | "rabbitmq" | "s3" | ...
	Version string `json:"version,omitempty"` // major or constraint, e.g. "16", ">=14"
	// ExposeAs maps logical fields (host, port, user, password, url, ...)
	// to env-var names the app reads. The orchestration platform fills
	// each one once the sidecar is bound. Field names are advisory but
	// the well-known set is "host", "port", "user", "password", "url",
	// "database", "vhost", "exchange".
	ExposeAs map[string]string `json:"exposeAs,omitempty"`
	// Optional indicates the app degrades gracefully without this
	// sidecar (e.g. a Redis cache that falls back to in-memory). The
	// platform may skip provisioning in dev environments.
	Optional bool `json:"optional,omitempty"`
}

// Volume describes a path inside the container that must persist.
// Shared=true tells orchestration this volume must be mounted from a
// shared backing store when the app is scaled horizontally (e.g. local
// uploads dir read by every replica). Single-replica apps can ignore
// the flag.
type Volume struct {
	Path    string `json:"path"`
	Purpose string `json:"purpose,omitempty"`
	Shared  bool   `json:"shared,omitempty"`
}

// StartupTask is a one-shot job run before the HTTP listener binds.
// Migrations are the canonical case. The Run function executes inside
// the app process; the orchestration platform doesn't see Run, only
// the declared name + phase, so it can decide whether to gate the
// rollout on success ("don't promote until startup tasks pass").
type StartupTask struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	// Phase is "pre-start" today; reserved for "post-start", "pre-stop"
	// future expansion. Encoded as a string for forward compatibility.
	Phase string                                       `json:"phase"`
	Run   func() error                                 `json:"-"`
}

// WorkerSummary, CronSummary, EndpointSummary mirror what /__nexus/workers,
// /__nexus/crons, /__nexus/endpoints already return — flattened to the
// minimum a deployer needs (no health, no history, no per-call stats).
// The framework fills these from the registry; apps don't construct them
// directly.
type WorkerSummary struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type CronSummary struct {
	Name     string `json:"name"`
	Schedule string `json:"schedule"`
}

type EndpointSummary struct {
	Service   string `json:"service,omitempty"`
	Transport string `json:"transport"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path"`
}

// ── Provider interfaces ────────────────────────────────────────────
//
// Mirror the existing automount.NexusResourceProvider pattern: small
// interfaces a module implements when it has something to declare.
// Detection is by reflection at boot (when wiring lands in nexus.go);
// nothing here imports nexus or fx, keeping this package free of
// import cycles.

// EnvProvider is implemented by anything that reads configuration from
// the environment and wants those reads documented in /__nexus/manifest.
// Typical implementer: a *Config struct's constructor, or a manager
// like *cache.Manager whose Start() reads REDIS_HOST/PORT/PASSWORD.
type EnvProvider interface {
	NexusEnv() []EnvVar
}

// ServiceDependencyProvider is implemented by anything that declares
// a logical sidecar requirement. Returning multiple is fine — a single
// "events" module might need both RabbitMQ (queue) and Postgres
// (idempotency table).
type ServiceDependencyProvider interface {
	NexusServices() []ServiceNeed
}

// VolumeProvider is implemented by anything that needs a writable
// path. Most apps won't implement this — they'll register volumes
// directly via app.UseVolume(...) once that hook lands.
type VolumeProvider interface {
	NexusVolumes() []Volume
}

// ── Aggregation ────────────────────────────────────────────────────

// Inputs is the data the framework collects from various places before
// rendering the manifest. Building it lives outside this package (the
// *App has the references); we accept it as a struct so this package
// has no dependency on nexus internals — meaning it stays unit-testable
// in isolation and won't create an import cycle.
//
// Most fields are slices the framework appends to as providers are
// discovered. Echoed sections (Workers/Crons/Endpoints) are read-only
// snapshots from the registry.
type Inputs struct {
	Name       string
	Version    string
	Deployment string
	Ports      []Port
	AppHealth  string // optional app-level probe path; "" omits the field

	EnvProviders     []EnvProvider
	ServiceProviders []ServiceDependencyProvider
	VolumeProviders  []VolumeProvider
	StartupTasks     []StartupTask

	// Direct registrations bypass the provider walk — used by
	// app.UseVolume(...) and any future app.DeclareEnv(...) calls
	// that don't go through an interface.
	DirectEnv      []EnvVar
	DirectServices []ServiceNeed
	DirectVolumes  []Volume

	Workers   []WorkerSummary
	Crons     []CronSummary
	Endpoints []EndpointSummary
}

// Build aggregates a Manifest from Inputs. Deduplicates env vars by
// Name (last writer wins on metadata fields, but Required is OR-ed
// across declarations and Secret is OR-ed too — once flagged secret,
// always secret). Sorts everything for deterministic output so
// /__nexus/manifest is stable across boots and easy to diff in CI.
func Build(in Inputs) Manifest {
	m := Manifest{
		Name:       in.Name,
		Version:    in.Version,
		Deployment: in.Deployment,
		Ports:      sortedPorts(in.Ports),
		Health: Health{
			Liveness:  "/__nexus/health",
			Readiness: "/__nexus/ready",
			App:       in.AppHealth,
		},
		Workers:   in.Workers,
		Crons:     in.Crons,
		Endpoints: in.Endpoints,
	}

	// Env: walk providers, then merge direct registrations. Dedup by Name.
	envByName := map[string]EnvVar{}
	mergeEnv := func(e EnvVar) {
		if e.Name == "" {
			return
		}
		prev, ok := envByName[e.Name]
		if !ok {
			envByName[e.Name] = e
			return
		}
		// Field-level merge: required/secret are sticky-true; description
		// / default / boundTo prefer the latest non-empty value.
		prev.Required = prev.Required || e.Required
		prev.Secret = prev.Secret || e.Secret
		if e.Description != "" {
			prev.Description = e.Description
		}
		if e.Default != "" {
			prev.Default = e.Default
		}
		if e.BoundTo != "" {
			prev.BoundTo = e.BoundTo
		}
		envByName[e.Name] = prev
	}
	for _, p := range in.EnvProviders {
		if p == nil {
			continue
		}
		for _, e := range p.NexusEnv() {
			mergeEnv(e)
		}
	}
	for _, e := range in.DirectEnv {
		mergeEnv(e)
	}
	if len(envByName) > 0 {
		m.Env = make([]EnvVar, 0, len(envByName))
		for _, e := range envByName {
			m.Env = append(m.Env, e)
		}
		sort.Slice(m.Env, func(i, j int) bool { return m.Env[i].Name < m.Env[j].Name })
	}

	// Services: dedup by Name. Two providers naming the same logical
	// sidecar is an authoring bug, but we keep the first declaration
	// rather than panicking — boot should not break on a metadata clash.
	svcByName := map[string]ServiceNeed{}
	addSvc := func(s ServiceNeed) {
		if s.Name == "" {
			return
		}
		if _, ok := svcByName[s.Name]; ok {
			return
		}
		svcByName[s.Name] = s
	}
	for _, p := range in.ServiceProviders {
		if p == nil {
			continue
		}
		for _, s := range p.NexusServices() {
			addSvc(s)
		}
	}
	for _, s := range in.DirectServices {
		addSvc(s)
	}
	if len(svcByName) > 0 {
		m.Services = make([]ServiceNeed, 0, len(svcByName))
		for _, s := range svcByName {
			m.Services = append(m.Services, s)
		}
		sort.Slice(m.Services, func(i, j int) bool { return m.Services[i].Name < m.Services[j].Name })
	}

	// Volumes: dedup by Path.
	volByPath := map[string]Volume{}
	addVol := func(v Volume) {
		if v.Path == "" {
			return
		}
		if _, ok := volByPath[v.Path]; ok {
			return
		}
		volByPath[v.Path] = v
	}
	for _, p := range in.VolumeProviders {
		if p == nil {
			continue
		}
		for _, v := range p.NexusVolumes() {
			addVol(v)
		}
	}
	for _, v := range in.DirectVolumes {
		addVol(v)
	}
	if len(volByPath) > 0 {
		m.Volumes = make([]Volume, 0, len(volByPath))
		for _, v := range volByPath {
			m.Volumes = append(m.Volumes, v)
		}
		sort.Slice(m.Volumes, func(i, j int) bool { return m.Volumes[i].Path < m.Volumes[j].Path })
	}

	if len(in.StartupTasks) > 0 {
		m.StartupTasks = append(m.StartupTasks, in.StartupTasks...)
		sort.Slice(m.StartupTasks, func(i, j int) bool { return m.StartupTasks[i].Name < m.StartupTasks[j].Name })
	}

	return m
}

func sortedPorts(in []Port) []Port {
	if len(in) == 0 {
		return nil
	}
	out := append([]Port(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// ── Print mode (the only consumer surface) ─────────────────────────
//
// The manifest is intentionally NOT served over HTTP. Print mode is
// the supported way for the orchestration platform to read it —
// invoked at upload / git-sync time, after the project's image has
// been built, via `docker run --rm -e NEXUS_PRINT_MANIFEST=1 <image>`.
//
// Reasons (vs. an HTTP endpoint):
//   - Manifest is needed BEFORE the app runs (to plan sidecars / env
//     / migrations), so a runtime endpoint is too late.
//   - No public network surface to defend → no auth gate to maintain.
//   - Build-time output, so it can be diffed in CI to catch breaking
//     deploy-config changes before merge.

// PrintJSON writes the manifest as pretty-printed JSON to w.
// Used by nexus.Run when EnvVarPrintAndExit is set; callable directly
// by tests / tooling that wants the same output without the os.Exit.
func PrintJSON(w io.Writer, m Manifest) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// ── Integration (sequenced; this file is step 0) ───────────────────
//
// 1. nexus.go: extend *App with synchronous, side-effect-free
//    registration methods so module-level options can declare without
//    invoking any constructor:
//
//      func (a *App) DeclareEnv(EnvVar)
//      func (a *App) DeclareEnvProvider(EnvProvider)
//      func (a *App) DeclareService(ServiceNeed)
//      func (a *App) DeclareServiceProvider(ServiceDependencyProvider)
//      func (a *App) UseVolume(Volume)
//      func (a *App) AddStartupTask(StartupTask)
//
//    plus an unexported manifestInputs() method that gathers
//    registered providers, registry snapshots, and listener ports
//    into manifest.Inputs.
//
// 2. options.go (Run): before fx.New(...).Run(), check
//    os.Getenv(EnvVarPrintAndExit) == "1". If yes: build the fx
//    app, fx.Populate the *App, call PrintJSON(os.Stdout,
//    Build(app.manifestInputs())), os.Exit(0). registerLifecycle's
//    OnStart never fires in this path — listeners stay unbound, no
//    DB/Redis/queue probing happens.
//
// 3. integration.go: when print mode is OFF, registerLifecycle
//    additionally runs registered StartupTasks on OnStart BEFORE
//    binding listeners. First error halts boot with the task name.
//
// 4. cache/cache.go (and ratelimit, future db): module-level
//    declarations so they register at module-construction time,
//    e.g. nexus.Module("cache", nexus.DeclareEnv(EnvVar{...}), ...).
//    Implementations of EnvProvider / ServiceDependencyProvider /
//    VolumeProvider must be side-effect-free — print mode walks them
//    before any constructor side-effect would have happened.
//
// 5. Orchestration platform builder: after a successful image build,
//    `docker run --rm -e NEXUS_PRINT_MANIFEST=1 <imageRef>`, capture
//    stdout, persist as Build.Manifest. Deploy planner reads it
//    offline to provision sidecars / render env / run startup tasks /
//    mount volumes — replacing most of what a recipe would otherwise
//    have to encode.
