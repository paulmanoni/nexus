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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"time"
)

// SchemaV1 is the current manifest schema version emitted by Build.
// Consumers (orchestrators, CI, dashboards) gate on the major: a "1.x"
// manifest is guaranteed to be readable by any v1-aware consumer.
// Additive changes bump the minor (1.0 → 1.1); field removal or shape
// change bumps the major and is announced.
const SchemaV1 = "1"

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
//
// Field ordering: SchemaVersion + ManifestHash come first so consumers
// reading the JSON head can gate on version + diff on hash without
// parsing the whole document. App identity follows. Then the v0
// back-compat top-level fields (Name/Version/Deployment, Ports,
// Health, Env, Services, Volumes, StartupTasks, Workers, Crons,
// Endpoints) — kept populated so existing readers (orchestrators
// already in production) stay green. Finally the v1 structured
// sections (Deployments, Modules, Routes, Entities, Frontend, Admin)
// — new readers consume these in preference to the flat back-compat
// fields.
type Manifest struct {
	// Schema + integrity ───────────────────────────────────────────
	// SchemaVersion is the contract version (see SchemaV1). Always
	// emitted; absence in a parsed document means the producer is
	// pre-v1 and the consumer should treat the document as best-effort.
	SchemaVersion string `json:"schemaVersion"`
	// ManifestHash is sha256 hex over the canonical JSON of every
	// other field, EXCEPT App.GeneratedAt (which would otherwise make
	// every emission unique). Stable across re-emissions of the same
	// build, so an orchestrator can use it as a cache key and a
	// "redeploy needed?" signal. Format: "sha256:<hex>".
	ManifestHash string `json:"manifestHash,omitempty"`

	// App identity ─────────────────────────────────────────────────
	// App is the structured replacement for the flat Name/Version/
	// Deployment fields. Both are populated for back-compat; v2 will
	// drop the flat fields. NexusVersion + GeneratedAt are new and
	// have no flat equivalent.
	App AppIdentity `json:"app"`

	// Identity (v0 back-compat — duplicated into App above) ────────
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

	// v1 structured sections ───────────────────────────────────────
	// New readers consume these in preference to the flat back-compat
	// fields above. Empty-or-nil when no provider has supplied data
	// yet; framework plumbing populates them as registry/topology
	// information becomes available.

	// Deployments is the topology declared in nexus.deploy.yaml: every
	// deployment unit, what modules it owns, its peers, scaling hints.
	// The framework's print mode does not read nexus.deploy.yaml — the
	// `nexus build --emit-manifest` tool merges the YAML topology into
	// the manifest before writing it out. Single-deployment ("monolith")
	// apps still get one entry here for uniformity.
	Deployments []Deployment `json:"deployments,omitempty"`

	// Modules is the per-module view: which deployment owns each module
	// and what routes/crons/entities it contributes. The Routes/Crons/
	// Entities fields here hold IDs into the top-level slices so the
	// orchestrator can filter without walking nested structures.
	Modules []Module `json:"modules,omitempty"`

	// Routes is the enriched view of every HTTP/GraphQL/WS surface,
	// including module + deployment tags and auth/tenant metadata.
	// Supersedes Endpoints; both are populated for back-compat.
	Routes []Route `json:"routes,omitempty"`

	// Entities is the crud-registered type catalog: name, fields,
	// supported operations, tenant scope. Drives the dashboard's
	// per-tenant data browser.
	Entities []Entity `json:"entities,omitempty"`

	// Frontend is non-nil when the app serves an embedded SPA
	// (nexus.ServeFrontend). The orchestrator uses BuildHash to gate
	// CDN invalidation on actual asset changes.
	Frontend *Frontend `json:"frontend,omitempty"`

	// Admin lists the framework-owned admin URL paths. Self-describing
	// so the orchestrator does not hardcode "/__nexus/..." — if a
	// future framework version moves them, the manifest is the source
	// of truth.
	Admin AdminPaths `json:"admin"`
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

// ── v1 structured types ────────────────────────────────────────────

// AppIdentity is the structured replacement for the flat Name/Version/
// Deployment fields on Manifest. Carries the framework version it was
// built against and the wall-clock timestamp the manifest was emitted —
// useful for "when did this binary report itself?" but excluded from
// ManifestHash so two emissions of the same build hash-equal.
type AppIdentity struct {
	Name string `json:"name"`
	// Version is the app's own version (typically `git describe` or a
	// build-time -ldflags injection). Empty when the app didn't set one.
	Version string `json:"version,omitempty"`
	// NexusVersion is the framework version the binary was built against.
	// Lets the orchestrator gate on framework features ("can this app
	// answer /__nexus/drain?") without sniffing endpoints.
	NexusVersion string `json:"nexusVersion,omitempty"`
	// GeneratedAt is the wall-clock UTC time of manifest emission, in
	// RFC3339. Excluded from ManifestHash.
	GeneratedAt string `json:"generatedAt,omitempty"`
}

// Deployment is one unit in the app's topology. A monolith app emits a
// single Deployment named "monolith"; a split app emits one per
// nexus.deploy.yaml `deployments:` entry.
type Deployment struct {
	Name string `json:"name"` // matches NEXUS_DEPLOYMENT
	Port int    `json:"port,omitempty"`
	// Owns is the list of module names assigned to this deployment.
	// Modules with no DeployAs tag and no manifest assignment are
	// "always local" — they appear under whichever deployment is active
	// at runtime, and so are not listed here.
	Owns []string `json:"owns,omitempty"`
	// Peers is the list of OTHER deployment names this deployment
	// declares as hard dependencies (must be reachable for ready=true).
	Peers []string `json:"peers,omitempty"`
	// Scaling is operator-supplied hints. Empty/zero values mean
	// "platform default" — the orchestrator picks min=max=1.
	Scaling Scaling `json:"scaling,omitzero"`
}

// Scaling is the v1 scaling hint shape. Kept intentionally small so
// the contract stays stable; richer policies (CPU/memory/queue-depth
// triggers) are deferred until the scaler module has real policies and
// can extend this struct additively.
type Scaling struct {
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
}

// Module is the per-module view of an app. The Routes/Crons/Entities
// fields hold IDs into the top-level Manifest.Routes/Crons/Entities
// slices — flat references over nesting so the orchestrator can index
// once and filter cheaply.
type Module struct {
	Name string `json:"name"`
	// Deployment is the resolved DeployAs tag (explicit or inferred from
	// nexus.deploy.yaml `owns`). Empty string means "always local" —
	// runs in whichever deployment is active.
	Deployment string `json:"deployment,omitempty"`
	// Package is the Go import path of the module, used for dashboard
	// deep-links to source. Empty when the framework can't infer it.
	Package  string   `json:"package,omitempty"`
	Routes   []string `json:"routes,omitempty"`   // route IDs
	Crons    []string `json:"crons,omitempty"`    // cron names
	Entities []string `json:"entities,omitempty"` // entity names
}

// Route is the enriched HTTP/GraphQL/WS surface descriptor. Supersedes
// EndpointSummary, which is kept on Manifest for back-compat. ID is
// stable across re-emissions of the same build (computed from
// kind+method+path+operation) so cross-references from Module.Routes
// stay valid.
type Route struct {
	ID string `json:"id"`
	// Kind is the route taxonomy:
	//   "rest"               — Method + Path
	//   "graphql.query"      — Operation
	//   "graphql.mutation"   — Operation
	//   "graphql.subscription" — Operation
	//   "ws"                 — Path
	Kind       string `json:"kind"`
	Module     string `json:"module,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	Method     string `json:"method,omitempty"`    // REST only
	Path       string `json:"path,omitempty"`      // REST/WS only
	Operation  string `json:"operation,omitempty"` // GraphQL only
	// Auth is "none" | "optional" | "required". Empty defaults to
	// "none" on the consumer side; emitters should set it explicitly.
	Auth         string `json:"auth,omitempty"`
	TenantScoped bool   `json:"tenantScoped,omitempty"`
}

// Entity is one crud-registered type. Drives the dashboard's
// per-tenant data browser and the orchestrator's "schema changed"
// diff-on-deploy view.
type Entity struct {
	Name         string        `json:"name"`
	Module       string        `json:"module,omitempty"`
	TenantScoped bool          `json:"tenantScoped,omitempty"`
	Ops          []string      `json:"ops,omitempty"` // "create","read","update","delete","list"
	Fields       []EntityField `json:"fields,omitempty"`
}

// EntityField is one column on an Entity. v1 captures name + type +
// the three flags an operator-facing data browser needs (PK to render
// row identity, Indexed/Unique to hint at safe filter columns).
// Foreign keys / cascading rules are deferred until v1.x — no consumer
// needs them today and modeling them well requires lock-in on the
// crud package's relation API.
type EntityField struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	PrimaryKey bool   `json:"primaryKey,omitempty"`
	Indexed    bool   `json:"indexed,omitempty"`
	Unique     bool   `json:"unique,omitempty"`
	Nullable   bool   `json:"nullable,omitempty"`
}

// Frontend describes an embedded SPA served by nexus.ServeFrontend.
// Absent (Manifest.Frontend == nil) when the app serves no frontend —
// don't emit an empty Frontend{} struct, the omitempty on Manifest
// handles it.
type Frontend struct {
	Embedded  bool   `json:"embedded"`
	MountPath string `json:"mountPath,omitempty"`
	// BuildHash is content hash of the embedded asset bundle. Empty
	// when the build pipeline didn't inject one. Orchestrator uses it
	// to decide whether a CDN invalidation is needed across deploys.
	BuildHash string `json:"buildHash,omitempty"`
}

// AdminPaths is the framework-owned URL surface the orchestrator drives.
// Self-describing so the orchestrator does not hardcode "/__nexus/...":
// if a future framework version moves them, this struct's emitted
// values change and the orchestrator follows.
type AdminPaths struct {
	ManifestPath string `json:"manifestPath,omitempty"`
	HealthPath   string `json:"healthPath,omitempty"`
	ReadyPath    string `json:"readyPath,omitempty"`
	MetricsPath  string `json:"metricsPath,omitempty"`
	DrainPath    string `json:"drainPath,omitempty"`
	ReloadPath   string `json:"reloadPath,omitempty"`
}

// DefaultAdminPaths returns the framework's current admin URL layout.
// Build() uses this when Inputs.Admin has zero values, so any consumer
// that doesn't override gets the canonical paths. Callers that need to
// customize (e.g. a test that mounts admin under a different prefix)
// pass an Inputs.Admin with the relevant fields set.
func DefaultAdminPaths() AdminPaths {
	return AdminPaths{
		ManifestPath: "/__nexus/manifest",
		HealthPath:   "/__nexus/health",
		ReadyPath:    "/__nexus/ready",
		MetricsPath:  "/__nexus/metrics",
		DrainPath:    "/__nexus/drain",
		ReloadPath:   "/__nexus/reload",
	}
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

// Registrar is the contract *nexus.App satisfies for the manifest
// declaration methods. Carved into the leaf manifest package so
// any package (cache, db, app-defined wrappers) can require it as
// an fx dependency without importing nexus and creating an import
// cycle.
//
// Implementations must accept nil-safe registration: calling with
// a nil provider is a no-op, not a panic. (Mirrors the *App method
// guards.)
type Registrar interface {
	DeclareEnvProvider(EnvProvider)
	DeclareServiceProvider(ServiceDependencyProvider)
	DeclareVolumeProvider(VolumeProvider)
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

	// NexusVersion is the framework version the binary was built
	// against. The framework injects it (typically via -ldflags or a
	// build-time constant in the nexus package); leaving it empty is
	// fine for tests.
	NexusVersion string

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

	// v1 structured inputs ─────────────────────────────────────────
	// The framework will populate these as registry/topology
	// integration lands; leaving them nil is supported and produces
	// empty top-level slices on the resulting Manifest.
	Deployments []Deployment
	Modules     []Module
	Routes      []Route
	Entities    []Entity
	// Frontend is non-nil when the app called nexus.ServeFrontend.
	Frontend *Frontend
	// Admin overrides DefaultAdminPaths(). Zero value (all fields
	// empty) means "use defaults"; setting any field overrides only
	// that path. Build() merges Admin field-by-field with defaults.
	Admin AdminPaths
}

// Build aggregates a Manifest from Inputs. Deduplicates env vars by
// Name (last writer wins on metadata fields, but Required is OR-ed
// across declarations and Secret is OR-ed too — once flagged secret,
// always secret). Sorts everything for deterministic output so
// /__nexus/manifest is stable across boots and easy to diff in CI.
//
// SchemaVersion is set to SchemaV1, App identity is filled from
// Inputs (with GeneratedAt = now UTC), AdminPaths fall back to
// DefaultAdminPaths() field-by-field, and ManifestHash is computed
// last over the canonical JSON of every other field (excluding
// App.GeneratedAt, so two emissions of the same build hash-equal).
func Build(in Inputs) Manifest {
	m := Manifest{
		SchemaVersion: SchemaV1,
		App: AppIdentity{
			Name:         in.Name,
			Version:      in.Version,
			NexusVersion: in.NexusVersion,
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		},
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
		Frontend:  in.Frontend,
		Admin:     mergeAdminPaths(in.Admin),
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

	// v1 structured sections. Each is deduped by its natural key,
	// then sorted, mirroring the older sections above.
	m.Deployments = sortedDeployments(in.Deployments)
	m.Modules = sortedModules(in.Modules)
	m.Routes = sortedRoutes(in.Routes)
	m.Entities = sortedEntities(in.Entities)

	// Hash last — every other field must be settled first. App.GeneratedAt
	// is excluded by ComputeHash so the hash is stable across re-emissions
	// of the same build.
	m.ManifestHash = ComputeHash(m)
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

// mergeAdminPaths fills in framework defaults for any AdminPaths field
// the caller left empty. The Inputs.Admin shape is "override-by-field":
// callers set only what they want to change. Done as field-by-field
// rather than reflect-based merge so the contract is grep-able and
// consumers know exactly which fields participate.
func mergeAdminPaths(override AdminPaths) AdminPaths {
	defaults := DefaultAdminPaths()
	if override.ManifestPath != "" {
		defaults.ManifestPath = override.ManifestPath
	}
	if override.HealthPath != "" {
		defaults.HealthPath = override.HealthPath
	}
	if override.ReadyPath != "" {
		defaults.ReadyPath = override.ReadyPath
	}
	if override.MetricsPath != "" {
		defaults.MetricsPath = override.MetricsPath
	}
	if override.DrainPath != "" {
		defaults.DrainPath = override.DrainPath
	}
	if override.ReloadPath != "" {
		defaults.ReloadPath = override.ReloadPath
	}
	return defaults
}

// sortedDeployments dedups by Name (first writer wins, matching the
// services dedup policy above — a duplicate declaration is an
// authoring bug we surface by keeping the first rather than panicking)
// and sorts by Name. Inner slices (Owns, Peers) are sorted too so two
// Build calls on equivalent inputs hash-equal regardless of caller
// ordering.
func sortedDeployments(in []Deployment) []Deployment {
	if len(in) == 0 {
		return nil
	}
	byName := make(map[string]Deployment, len(in))
	for _, d := range in {
		if d.Name == "" {
			continue
		}
		if _, ok := byName[d.Name]; ok {
			continue
		}
		d.Owns = sortedStrings(d.Owns)
		d.Peers = sortedStrings(d.Peers)
		byName[d.Name] = d
	}
	out := make([]Deployment, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedModules dedups by Name and sorts. Inner ID/name slices are
// sorted for hash stability.
func sortedModules(in []Module) []Module {
	if len(in) == 0 {
		return nil
	}
	byName := make(map[string]Module, len(in))
	for _, m := range in {
		if m.Name == "" {
			continue
		}
		if _, ok := byName[m.Name]; ok {
			continue
		}
		m.Routes = sortedStrings(m.Routes)
		m.Crons = sortedStrings(m.Crons)
		m.Entities = sortedStrings(m.Entities)
		byName[m.Name] = m
	}
	out := make([]Module, 0, len(byName))
	for _, m := range byName {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedRoutes dedups by ID and sorts. Routes with empty ID are
// dropped — the ID is the cross-reference key from Module.Routes and
// a route nobody can reference is dead weight.
func sortedRoutes(in []Route) []Route {
	if len(in) == 0 {
		return nil
	}
	byID := make(map[string]Route, len(in))
	for _, r := range in {
		if r.ID == "" {
			continue
		}
		if _, ok := byID[r.ID]; ok {
			continue
		}
		byID[r.ID] = r
	}
	out := make([]Route, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// sortedEntities dedups by Name. Field order is preserved as declared
// (column order is meaningful for a data browser); the per-entity Ops
// slice is sorted so two equivalent declarations hash-equal.
func sortedEntities(in []Entity) []Entity {
	if len(in) == 0 {
		return nil
	}
	byName := make(map[string]Entity, len(in))
	for _, e := range in {
		if e.Name == "" {
			continue
		}
		if _, ok := byName[e.Name]; ok {
			continue
		}
		e.Ops = sortedStrings(e.Ops)
		byName[e.Name] = e
	}
	out := make([]Entity, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedStrings returns a sorted, dup-free copy of in. Returns nil for
// empty input so the resulting JSON omits the field cleanly.
func sortedStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ComputeHash returns the canonical content hash of m, suitable for
// "did the manifest change between these two builds?" checks. The hash
// EXCLUDES ManifestHash itself (so it doesn't depend on its own value)
// and App.GeneratedAt (so re-emitting the same build produces the same
// hash). Returned in "sha256:<hex>" form to match common content-addressed
// schemes; downstream tools can string-compare without parsing.
//
// Determinism rests on three things, all already true elsewhere in
// Build():
//   - struct fields are encoded in declaration order by encoding/json,
//   - all manifest slices are sorted before this is called,
//   - manifest maps (only EnvVar.ExposeAs etc.) are sorted by key by
//     Go's encoder since 1.12.
//
// If a future field stores a non-deterministic shape (random IDs,
// timestamps), exclude it here the same way GeneratedAt is.
func ComputeHash(m Manifest) string {
	m.ManifestHash = ""
	m.App.GeneratedAt = ""
	buf, err := json.Marshal(m)
	if err != nil {
		// json.Marshal of these types only fails on cycles or
		// unsupported kinds, neither of which the schema admits.
		// Returning the empty string surfaces the failure to readers
		// without panicking the print path.
		return ""
	}
	sum := sha256.Sum256(buf)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ── Consumer surfaces ──────────────────────────────────────────────
//
// The same Manifest value is emitted from three places, all consuming
// Build() output so they cannot drift:
//
//  1. Print mode — the build-time path, kept canonical for the
//     orchestration platform's plan step. The platform invokes the
//     built binary with NEXUS_PRINT_MANIFEST=1 and captures stdout
//     (no docker required; `nexus reconcile` accepts --binary /
//     --source / --manifest-json directly). Build-time output is also
//     diff-able in CI so breaking deploy-config changes show up
//     pre-merge.
//
//  2. `nexus build --emit-manifest=manifest.json` — the build-tool
//     path. Same Build() output, written next to the binary as part
//     of the build pipeline. Lets the orchestrator render a deploy
//     preview before the binary boots.
//
//  3. GET /__nexus/manifest (planned) — the runtime path, gated by an
//     orchestrator-provisioned admin token. Lets a running fleet
//     answer "what shape am I?" without rebuild. The framework's
//     dashboard mounts it; ManifestHash lets the orchestrator detect
//     the rare build-time vs runtime drift case.
//
// Print mode remains the contract that newcomers learn first because
// it's the simplest: one env var, one stdout read, no auth surface.
// The HTTP endpoint is a convenience for already-running fleets, not
// a replacement.

// PrintJSON writes the manifest as pretty-printed JSON to w.
// Used by nexus.Run when EnvVarPrintAndExit is set; callable directly
// by tests / tooling that wants the same output without the os.Exit.
//
// Output is wrapped in BeginMarker / EndMarker sentinels on their own
// lines so parsers can extract the manifest from a stream that also
// carries unrelated stdout (a user's stdlib log / zap / zerolog
// emissions during fx graph construction). ExtractJSON walks any
// io.Reader and pulls out the bytes between the markers; absent
// markers, callers fall back to treating the whole stream as JSON
// (the v0 contract — preserved for binaries built against earlier
// nexus releases).
func PrintJSON(w io.Writer, m Manifest) error {
	if _, err := io.WriteString(w, BeginMarker+"\n"); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	_, err := io.WriteString(w, EndMarker+"\n")
	return err
}

// BeginMarker / EndMarker bracket the JSON in print-mode output.
// Each is on its own line; Extract reads the bytes between (exclusive
// of the markers themselves). Picked for low collision probability
// with realistic log output — neither string is plausible as a log
// prefix or JSON content.
const (
	BeginMarker = "===BEGIN-NEXUS-MANIFEST==="
	EndMarker   = "===END-NEXUS-MANIFEST==="
)

// Extract pulls the manifest JSON bytes from a print-mode stream
// that may also contain stdout pollution (user stdlib log / zap /
// zerolog lines emitted during fx graph construction).
//
// Strategy:
//   - If both BeginMarker and EndMarker appear, return the bytes
//     between them (newlines preserved). This is the v1+ contract.
//   - Else, if no markers appear, return the whole input. This is
//     the v0 fallback — old nexus binaries that emit raw JSON keep
//     working with new parsers.
//   - Else (only one marker), return an error — partial output is
//     more likely a truncation bug than a real manifest, and
//     guessing risks parsing junk.
func Extract(raw []byte) ([]byte, error) {
	bi := bytesIndex(raw, []byte(BeginMarker))
	ei := bytesIndex(raw, []byte(EndMarker))
	if bi < 0 && ei < 0 {
		// No markers at all → assume v0 raw-JSON output.
		return raw, nil
	}
	if bi < 0 || ei < 0 || ei <= bi {
		return nil, errBadMarkers
	}
	start := bi + len(BeginMarker)
	// Skip the trailing newline after the begin marker (and the one
	// before the end marker) so the extracted slice is just the JSON.
	for start < ei && (raw[start] == '\n' || raw[start] == '\r') {
		start++
	}
	end := ei
	for end > start && (raw[end-1] == '\n' || raw[end-1] == '\r') {
		end--
	}
	return raw[start:end], nil
}

// errBadMarkers signals that print-mode output contains exactly one
// marker — likely a truncation rather than a complete manifest.
var errBadMarkers = errMarkers{}

type errMarkers struct{}

func (errMarkers) Error() string {
	return "manifest: print-mode output has only one of BEGIN/END markers — output likely truncated"
}

// bytesIndex is bytes.Index without the import (this package is
// stdlib-only-by-design and bytes is fine, just keeping it explicit).
func bytesIndex(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
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
