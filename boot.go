package nexus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/dev"
	"github.com/paulmanoni/nexus/v2/di"
	"github.com/paulmanoni/nexus/v2/httpx"
	"github.com/paulmanoni/nexus/v2/internal/bootui"
	"github.com/paulmanoni/nexus/v2/manifest"
)

// Option composes a nexus app. Everything returned by Provide, Supply,
// Invoke, Module, AsRest, AsQuery, AsMutation, AsWebSocket, AsSubscription
// is an Option, ready to pass to Run. The DI container is an implementation
// detail — user code imports only nexus.
type Option interface{ nexusOption() di.Option }

// Lifecycle and Hook are re-exported from the di seam so extensions can take a
// lifecycle parameter and register start/stop hooks without importing di
// directly. The builtin container provides Lifecycle natively; the opt-in fx
// adapter bridges fx.Lifecycle onto it.
type (
	Lifecycle = di.Lifecycle
	Hook      = di.Hook
)

type rawOption struct{ o di.Option }

func (r rawOption) nexusOption() di.Option { return r.o }

// moduleOption is what Module returns: a rawOption that remembers its module
// name, so DecoratedModules can filter decorate-drained registrations without
// reaching into the di graph. Behaviorally identical to rawOption.
type moduleOption struct {
	name string
	o    di.Option
}

func (m moduleOption) nexusOption() di.Option { return m.o }

// routerOption carries a chosen HTTP router backend. It is consumed BEFORE the
// graph is built (Run scans for it and seeds Config.Router, since the router
// is constructed inside New(cfg) which runs ahead of user options). Its
// container contribution is therefore a no-op.
type routerOption struct{ r httpx.Router }

func (routerOption) nexusOption() di.Option { return di.Options() }

// containerOption carries a chosen DI backend. Like routerOption it is consumed
// before the graph is built (Run scans for it), so its own graph contribution
// is a no-op.
type containerOption struct{ backend di.Backend }

func (containerOption) nexusOption() di.Option { return di.Options() }

// WithContainer selects the dependency-injection backend (default: the
// zero-dependency builtin container in nexus/di). Pass the opt-in fx adapter to
// switch:
//
//	nexus.Boot(nexus.WithContainer(fxcontainer.New()))
//
// Selecting the fx adapter pulls go.uber.org/fx (and dig) into the build; the
// builtin default links none of it. Mirrors WithRouter.
func WithContainer(backend di.Backend) Option { return containerOption{backend: backend} }

// WithRouter selects the HTTP router backend (default: the zero-dependency
// stdlib net/http router). Pass an opt-in adapter to switch:
//
//	nexus.Boot(nexus.WithRouter(ginrouter.New()))
//	nexus.Run(cfg, nexus.WithRouter(chirouter.New()))
//
// Equivalent to setting Config.Router. One line, no nexus.toml plumbing, and
// trivial to change — selecting gin/chi pulls their deps into the build, while
// the default links no third-party router at all.
func WithRouter(r httpx.Router) Option { return routerOption{r: r} }

// Module groups options under a name. Mirrors di.Module's logging — the
// group name appears in startup/shutdown logs and in error messages, which
// helps when several modules touch the same service or resource. The name
// is also stamped onto every AsQuery/AsMutation/AsRest registration inside
// the module so the dashboard's architecture view can group endpoints by
// module container.
//
//	var ordersModule = nexus.Module("orders",
//	    nexus.Provide(NewOrdersService),
//	    nexus.AsQuery(NewListOrders),
//	    nexus.AsMutation(NewCreateOrder, …),
//	)
func Module(name string, opts ...Option) Option {
	// Collect any RoutePrefix declarations among the direct children
	// so we can stamp them on REST registrations. Multiple prefixes
	// in the same Module concatenate left-to-right:
	//   Module("x", RoutePrefix("/a"), RoutePrefix("/b"), ...) → "/a/b".
	//
	// PublicPath is consumed alongside RoutePrefix — it's a sugar
	// that means "this is the module's URL prefix" and must apply
	// to REST mounts the same way RoutePrefix does. It ALSO seeds
	// the module GraphQL path registry so app.Service(<modName>)
	// returns a Service rooted at <path>/graphql.
	var prefix string
	var publicPath string
	for _, o := range opts {
		if rp, ok := o.(routePrefixOption); ok {
			prefix += rp.prefix
		}
		if pp, ok := o.(pathOption); ok {
			// Use the normalized form here so "/" is treated as a
			// no-op prefix (existing module semantics) while
			// AsComponent's Apply still sees the raw "/" as a
			// literal root-URL mount.
			normalized := pp.normalizedPath()
			publicPath = normalized
			prefix += normalized
		}
	}
	// Register the module's GraphQL path BEFORE the children walk
	// below. Module-aware children read the registry indirectly
	// (via app.Service at construction time), so the registration
	// only needs to land before di.Start fires constructors —
	// which happens after this whole Module() call returns.
	if publicPath != "" {
		registerModulePublicPath(name, publicPath)
	}

	// Stamp module name + route prefix onto every child option that
	// cares. Options produced by nested Module(...) don't implement
	// these annotator interfaces (they return a rawOption wrapping
	// di.Module), so inner-most wins automatically — the inner
	// Module() already annotated its own children before we see it.
	for _, o := range opts {
		if ma, ok := o.(moduleAnnotator); ok {
			ma.setModule(name)
		}
		// A router expands into its own module, which would shadow this
		// one's prefix — so it takes the prefix itself.
		if ea, ok := o.(enclosingAnnotator); ok {
			ea.setEnclosing(prefix, publicPath)
			continue
		}
		if prefix != "" {
			if rp, ok := o.(restPrefixAnnotator); ok {
				rp.setRestPrefix(prefix)
			}
		}
	}
	return moduleOption{name: name, o: di.Module(name, unwrap(opts)...)}
}

// Options bundles multiple Option values into a single Option.
// Useful when one logical feature expands into several: a
// conditional gate that pulls in a frontend mount + a config
// supply + an extra invoke, for example. Empty input is a no-op.
func Options(opts ...Option) Option {
	if len(opts) == 0 {
		return rawOption{o: di.Options()}
	}
	return rawOption{o: di.Options(unwrap(opts)...)}
}

// moduleAnnotator is implemented by options that participate in the
// nexus.Module grouping — specifically AsQuery/AsMutation/AsRest. The
// Module() function walks its direct children and calls setModule on
// each implementer so the registered endpoint knows its module.
type moduleAnnotator interface {
	setModule(name string)
}

// Provide registers one or more constructor functions with the dep
// graph and auto-detects two opt-in extensions:
//
//   - Resource providers: any returned value implementing
//     NexusResourceProvider has its resource.Resource list registered
//     with the app at boot. Add UseReporter alongside and OnResourceUse
//     wires automatically — service→resource edges appear on first
//     UsingCtx call without manual plumbing.
//
//   - Service wrappers: when the first return is a *T whose struct
//     anonymously embeds *nexus.Service, the constructor's params are
//     scanned for resource providers and other service wrappers. The
//     resulting (resourceDeps, serviceDeps) lists are recorded on the
//     service's registry entry so the dashboard's architecture view
//     draws service→service and service→resource edges at the SERVICE
//     layer with no extra annotation.
//
// Constructors that don't trigger either detector behave like plain
// di.Provide — return types enter the graph, params resolve from it.
// Mixed sets (one service wrapper + one resource manager + one plain
// helper) work in a single call.
//
//	nexus.Provide(
//	    NewDBManager,        // resource provider — auto-registered
//	    NewCacheManager,     // ditto
//	    NewOrdersService,   // service wrapper — deps recorded
//	    NewClock,            // plain type — just enters the graph
//	)
func Provide(fns ...any) Option {
	opts := make([]di.Option, 0, len(fns)+1)
	opts = append(opts, di.Provide(fns...))
	for _, fn := range fns {
		if inv := resourceAutoRegisterInvoke(fn); inv != nil {
			opts = append(opts, inv)
		}
		if inv := serviceDepsRegisterInvoke(fn); inv != nil {
			opts = append(opts, inv)
		}
		if inv := manifestAutoRegisterInvoke(fn); inv != nil {
			opts = append(opts, inv)
		}
	}
	return rawOption{o: di.Options(opts...)}
}

// Supply puts concrete values into the graph (no constructor). Useful for
// config structs or pre-built instances created outside the fx graph.
//
//	nexus.Supply(config.Runtime{Server: config.Server{Addr: ":8080"}})   // rare — Run takes config.Runtime directly
//	nexus.Supply(myAlreadyBuiltClient)          // typical
func Supply(values ...any) Option {
	return rawOption{o: di.Supply(values...)}
}

// Error injects an error discovered while building options; it surfaces at boot
// instead of panicking at call time. Extensions use it to report bad config
// without importing the DI backend.
//
//	if err := cfg.validate(); err != nil { return nexus.Error(err) }
func Error(err error) Option { return rawOption{o: di.Error(err)} }

// Invoke runs a function at startup, resolving its parameters from the
// graph. Use for side-effects on boot — attaching resources, registering
// hooks, seeding state. Multiple Invoke options run in registration order.
//
//	nexus.Invoke(func(app *nexus.App, dbs *DBManager) {
//	    app.OnResourceUse(dbs)
//	})
func Invoke(fns ...any) Option {
	return rawOption{o: di.Invoke(fns...)}
}

// serviceDepsRegisterInvoke synthesizes an di.Invoke that takes the
// constructed service + ALL of the constructor's original params,
// walks them for NexusResourceProvider / service-wrapper values, and
// calls registry.SetServiceDeps with the resulting name lists.
// Returns nil when fn isn't a function or its return isn't a
// service wrapper — letting ProvideService degrade to a plain
// Provide without failing boot.
func serviceDepsRegisterInvoke(fn any) di.Option {
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func || rt.NumOut() == 0 {
		return nil
	}
	serviceType := rt.Out(0)
	if !isServiceWrapperType(serviceType) {
		return nil
	}
	// Invoke signature: (serviceType, param0, param1, ...) — fx will
	// resolve each from the graph the same way it resolved them for
	// the constructor itself.
	in := make([]reflect.Type, 0, rt.NumIn()+1)
	in = append(in, serviceType)
	for i := 0; i < rt.NumIn(); i++ {
		in = append(in, rt.In(i))
	}
	invokeType := reflect.FuncOf(in, nil, false)
	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		svc, ok := unwrapService(args[0], serviceType)
		if !ok || svc == nil {
			return nil
		}
		owning := svc.Name()

		var resourceDeps []string
		var serviceDeps []string
		// args[0] is the constructed service itself; args[1:] mirror
		// the constructor's declared params in order.
		for i := 1; i < len(args); i++ {
			argType := rt.In(i - 1)
			argVal := args[i]
			if !argVal.IsValid() {
				continue
			}
			if provider, ok := argVal.Interface().(NexusResourceProvider); ok {
				for _, r := range provider.NexusResources() {
					resourceDeps = append(resourceDeps, r.Name())
				}
			}
			if isServiceWrapperType(argType) {
				if depSvc, ok := unwrapService(argVal, argType); ok && depSvc != nil && depSvc.Name() != owning {
					serviceDeps = append(serviceDeps, depSvc.Name())
				}
			}
		}
		svc.app.Registry().SetServiceDeps(owning, resourceDeps, serviceDeps)
		return nil
	})
	return di.Invoke(invokeFn.Interface())
}

// resourceAutoRegisterInvoke synthesizes an di.Invoke(func(app *App, instance T))
// that, at boot, registers resources and wires OnResourceUse for the instance.
// Returns nil when fn isn't a function, returns nothing, or its first
// return type doesn't implement NexusResourceProvider or UseReporter —
// skipping the invoke avoids forcing a *App dep on the graph for plain
// types (a regression that surfaces when nexus.Provide is used for
// unrelated values like func() string in tests).
func resourceAutoRegisterInvoke(fn any) di.Option {
	return autoRegisterInvoke(fn,
		[]reflect.Type{
			reflect.TypeFor[NexusResourceProvider](),
			reflect.TypeFor[UseReporter](),
		},
		func(app *App, inst any) {
			if p, ok := inst.(NexusResourceProvider); ok {
				for _, r := range p.NexusResources() {
					app.Register(r)
				}
			}
			if reporter, ok := inst.(UseReporter); ok {
				app.OnResourceUse(reporter)
			}
		})
}

// autoRegisterInvoke synthesizes a di.Invoke of shape func(*App, T)
// for a constructor fn whose first return type implements at least one
// of ifaces, calling apply with the constructed instance. Returns nil
// when nothing matches so the caller skips the invoke. The shared core
// behind resourceAutoRegisterInvoke and manifestAutoRegisterInvoke.
func autoRegisterInvoke(fn any, ifaces []reflect.Type, apply func(app *App, inst any)) di.Option {
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func || rt.NumOut() == 0 {
		return nil
	}
	// First return is the constructed instance. Ignore trailing error return.
	outType := rt.Out(0)
	matched := false
	for _, it := range ifaces {
		if outType.Implements(it) {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	invokeType := reflect.FuncOf(
		[]reflect.Type{reflect.TypeOf((*App)(nil)), outType},
		nil, false,
	)
	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		apply(args[0].Interface().(*App), args[1].Interface())
		return nil
	})
	return di.Invoke(invokeFn.Interface())
}

// Raw is an escape hatch: accept any di.Option and route it through nexus.
// For low-level container wiring or one-off integrations. Normal apps never
// need it.
//
//	nexus.Raw(di.Provide(myCtor))
func Raw(opt di.Option) Option {
	return rawOption{o: opt}
}

// Boot loads nexus.toml automatically — the [runtime] Config, every
// [extensions.*] block, the [env] bridge, and the config.Get base
// layer — then runs the app. It's the zero-boilerplate form of:
//
//	cfg  := config.MustLoad()
//	ext := nexus.MustLoadExtensions()
//	nexus.Run(cfg, append(opts, userOpts...)...)
//
// so main() collapses to:
//
//	func main() {
//	    nexus.Boot(
//	        nexus.Frontend(webFS, "web/dist"),
//	        billing.Module,
//	    )
//	}
//
// A missing nexus.toml is tolerated (zero Config, no extensions) so
// apps without one still boot; a malformed one panics, matching the
// MustLoad* helpers. Override the path with the NEXUS_CONFIG env var,
// or call BootFrom(path, opts...).
//
// Run stays available for apps that build Config in Go or want
// explicit control over load order — Boot is sugar over it. Note that
// extension PACKAGES still need their blank import (Go links only
// imported code); Boot removes the load calls, not the imports.
func Boot(opts ...Option) {
	BootFrom(resolveConfigPath(), opts...)
}

// BootFrom is Boot with an explicit nexus.toml path.
func BootFrom(path string, opts ...Option) {
	cfg, extOpts := autoLoad(path)
	Run(cfg, append(extOpts, opts...)...)
}

// resolveConfigPath picks the nexus.toml path in priority order:
//
//  1. NEXUS_CONFIG env override — always wins when set.
//  2. config.DefaultPath ("nexus.toml") in the current working directory —
//     the dev-time convention (cwd == project root).
//  3. nexus.toml sitting next to the executable — the deploy convention.
//     A binary shipped with its config beside it (./myapp +
//     ./nexus.toml) then binds the configured port no matter which
//     directory it's launched from, instead of silently falling back to
//     the framework default (:8080) when cwd has no toml.
//
// The cwd copy is tried first so a `nexus dev` / `go run` from the project
// root keeps reading the source-tree toml even when a built binary also
// sits nearby.
func resolveConfigPath() string {
	if p := os.Getenv("NEXUS_CONFIG"); p != "" {
		return p
	}
	if _, err := os.Stat(config.DefaultPath); err == nil {
		return config.DefaultPath
	}
	if exe, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(exe), config.DefaultPath)
		if _, err := os.Stat(beside); err == nil {
			return beside
		}
	}
	// Nothing found anywhere — return the conventional path so autoLoad's
	// ErrNotExist branch runs (and warns) with a familiar name.
	return config.DefaultPath
}

// autoLoad reads runtime Config + extension options for Boot. It
// resolves the TOML source in priority order:
//
//  1. the disk file at path (NEXUS_CONFIG → cwd → next to the executable,
//     via resolveConfigPath) — an operator's on-disk config always wins,
//     so a deployed binary can be re-tuned without a rebuild;
//  2. the copy embedded at build time by `nexus build` (config/embed.go),
//     so a single self-contained binary carries its own defaults;
//  3. nothing — framework defaults, with a loud warning (a silently
//     dropped config was the classic "why is it on :8080?" footgun).
//
// A malformed config (disk or embedded) fails the boot with a structured
// diagnostic (internal/bootui/bootui.go) so misconfiguration surfaces loudly at
// startup — as an operator-readable block, not a panic trace.
func autoLoad(path string) (config.Runtime, []Option) {
	f, err := config.Read(path)
	if err != nil {
		bootui.Fatal(err)
	}
	if f == nil {
		// No config anywhere. Tolerated so config-less apps still boot —
		// but it silently drops every setting a file would carry (listen
		// addr included, so the app falls back to :8080). That has bitten
		// deployments launched from a directory without their toml, so
		// make it loud on stderr rather than a mystery default port.
		fmt.Fprintf(os.Stderr,
			"nexus: no %s found (looked in cwd, next to the executable, and the "+
				"build-time embed); using framework defaults — listen addr falls "+
				"back to :8080. Set NEXUS_CONFIG, run from the config's directory, "+
				"or `nexus build` to embed it.\n",
			config.DefaultPath)
		return config.Runtime{}, nil
	}
	extOpts, err := decodeExtensions(f.Raw)
	if err != nil {
		bootui.Fatal(config.NewError("decode [extensions.*]", f.Source, err))
	}
	// Dev boot self-check: run the same config lint `nexus lint` runs, but at
	// boot in dev, so a bad CIDR / CORS combo / rate limit / unimported
	// extension surfaces now instead of only when someone remembers to lint.
	// Advisory (reported by runBootChecks, never aborts); prod pays nothing.
	if dev.Enabled() {
		if issues, lerr := f.Lint(); lerr == nil {
			addPendingBootIssues(issues)
		}
	}
	return f.Runtime, extOpts
}

// Run starts an app from a Config you build in Go, plus the given options
// (modules, Provide, AsRest/AsQuery/AsWS, extension modules). It blocks until
// the process is signalled to stop.
//
// Most apps should call Boot instead — it loads Config + [extensions.*] from
// nexus.toml and is sugar over Run. Reach for Run when Config carries values
// TOML can't express (a shared Store, a middleware func slice, a pluggable
// router/container backend) or when you want explicit control over load order.
// For tests, use InProcess (no listener). See the package doc for the full
// entry-point rundown.
func Run(cfg config.Runtime, opts ...Option) {
	// Print-mode short-circuit. When NEXUS_PRINT_MANIFEST=1 is set,
	// the orchestration platform is invoking us at build/upload time
	// to extract the manifest. Build the fx graph, populate *App
	// (which fires every DeclareEnv / DeclareService / DeclareVolume /
	// AddStartupTask invoke from module-level options), print the
	// manifest as JSON, exit 0. Lifecycle hooks never run — no
	// listener bind, no DB/Redis dial.
	//
	// Side-effect contract: implementations of EnvProvider /
	// ServiceDependencyProvider / VolumeProvider, and any constructor
	// that fx invokes during graph build, must be cheap and free of
	// network/filesystem reads. fx is lazy by default, so this holds
	// for typical apps.
	if os.Getenv(printManifestEnv) == "1" {
		printManifestAndExitIfRequested(cfg, opts)
		return // unreachable; printManifestAndExitIfRequested calls os.Exit
	}
	// Two-phase split: fxEarlyOptions seeds Config + *App + lifecycle
	// BEFORE user opts run, then user opts (which may install global
	// middleware via auth.Module / engine.Use), then fxLateOptions
	// runs autoMountGraphQL last so GraphQL routes pick up every
	// user-installed middleware. Without the split, GraphQL routes
	// registered first wouldn't see middleware Use()'d afterwards
	// — gin captures middleware at route-registration time.
	//
	// autoManifestOptions sits between Early and the user opts so
	// any plugin the user declares can read its per-environment
	// block from app.EffectiveManifest() at boot without the
	// operator having to write a LoadDeployManifest invoke.
	// Resolve the router backend before the graph is built: New(cfg)
	// (inside fxEarlyOptions) constructs the default router, so a
	// WithRouter option must seed Config.Router up front.
	backend := di.Backend(di.Builtin())
	for _, o := range opts {
		if ro, ok := o.(routerOption); ok {
			cfg.Router = ro.r
		}
		if co, ok := o.(containerOption); ok && co.backend != nil {
			backend = co.backend
		}
	}
	all := append([]di.Option{
		fxEarlyOptions(cfg),
		autoManifestOptions(),
	}, unwrap(opts)...)
	// Deferred sources (e.g. nexus/decorate's //nexus:-annotation drain) contribute
	// AFTER the app's own options and BEFORE autoMountGraphQL, so their
	// endpoints take part in schema assembly like any hand-written module.
	all = append(all, unwrap(filterDeferredOptions(opts, collectDeferredOptions()))...)
	all = append(all, fxLateOptions())
	// Bound the whole stop chain, not just the HTTP drain. The listener
	// hook already caps its own Shutdown; this covers everything after
	// it (db/cache Close, worker cancel) so no single wedged resource can
	// hold the process open. Sized above the HTTP window so a normal
	// drain never trips it.
	all = append(all, Raw(di.WithStopTimeout(shutdownTimeout(cfg)+5*time.Second)).nexusOption())
	// Dev boot self-check runs LAST as an invoke — after pubsub's BindTopics
	// and every other wiring invoke — so live-topology checks (e.g. "topic has
	// no transport bound") see the finalized graph. Dev-only: no invoke, no
	// cost in production.
	if dev.Enabled() {
		all = append(all, Invoke(func() { runBootChecks() }).nexusOption())
		// Snapshot preserved in-memory state on the way out, so the binary
		// `nexus dev` is about to swap in can pick it up (see dev/state.go).
		// Registered last => runs first on shutdown, before resources close.
		all = append(all, Invoke(func(lc Lifecycle) {
			lc.Append(Hook{OnStop: func(context.Context) error {
				return dev.SaveState()
			}})
		}).nexusOption())
	}
	beginBuild()
	inst := backend.Build(di.Collect(all...))
	// Both backends need this. The fx adapter installs fx.NopLogger and Run()
	// never consults Err(), so a wiring failure there used to exit 1 with an
	// empty stderr; the builtin printed one flat line. Rendering here gives
	// either backend the same structured block, plus a fix line for the
	// framework types a developer never asked for by name.
	if err := inst.Err(); err != nil {
		bootui.Render(os.Stderr, &wiringError{err: err, hint: wiringHint(err)}, bootui.Colors())
		os.Exit(1)
	}
	inst.Run()
}

// wiringError carries a remediation hint alongside a wiring failure so
// renderBootError can print a "fix" row for something that is not a
// config.Error.
type wiringError struct {
	err  error
	hint string
}

func (e *wiringError) Error() string { return e.err.Error() }
func (e *wiringError) Unwrap() error { return e.err }
func (e *wiringError) Hint() string  { return e.hint }

// wiringHints maps a type named by a "no provider for" error to the line that
// fixes it. These are types the binders ask for on the app's behalf, so the
// developer has never written them down and the bare error reads as a puzzle.
// (The binders' *slog.Logger needs no entry: the framework provides it.)
var wiringHints = map[string]string{}

// loggerDupHint fixes the one collision a migrating app hits: providing its
// own *slog.Logger beside the framework's.
const loggerDupHint = "the framework provides *slog.Logger — pass yours with nexus.WithLogger(l) instead of Provide/Supply"

func wiringHint(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "*slog.Logger") && (strings.Contains(msg, "already") || strings.Contains(msg, "more than once")) {
		return loggerDupHint
	}
	for typ, hint := range wiringHints {
		if strings.Contains(msg, "no provider for "+typ) {
			return hint
		}
	}
	return ""
}

// unwrap flattens a []Option into the []di.Option the container needs.
func unwrap(opts []Option) []di.Option {
	out := make([]di.Option, len(opts))
	for i, o := range opts {
		out[i] = o.nexusOption()
	}
	return out
}

// deferredOptionSources are functions that yield Options at Boot/Run time —
// AFTER every package init() has run. This is the seam that lets nexus/decorate
// auto-wire //nexus:-annotated registrations without the app writing an explicit
// drain call: decorate registers its drain here from its own init(), and Run
// folds the result into the option tree. nexus never imports decorate, so the
// dependency points the safe way (decorate → nexus).
var deferredOptionSources []func() []Option

// RegisterDeferredOptions registers a source of Options collected at Boot/Run
// time. Sources run in registration order, inserted after the app's own
// options and before the GraphQL auto-mount, so decorator-registered endpoints
// participate in schema assembly exactly like hand-written ones.
//
// Intended for framework integration (nexus/decorate); apps don't call it.
func RegisterDeferredOptions(fn func() []Option) {
	if fn != nil {
		deferredOptionSources = append(deferredOptionSources, fn)
	}
}

// collectDeferredOptions invokes every registered source and concatenates the
// results. Run/print-mode call it once while building the option tree.
func collectDeferredOptions() []Option {
	var out []Option
	for _, fn := range deferredOptionSources {
		out = append(out, fn()...)
	}
	return out
}

// DecoratedModules limits which //nexus:-annotated (decorate-registered) modules
// this boot accepts: of the registrations the deferred sources drain, only
// top-level modules whose name is listed participate; everything else drained
// is dropped. Hand-written options are never affected, and without this
// option every drained registration participates, as before.
//
// It exists for tests. The decorate registry is process-global, so an
// InProcess boot in a test binary sees the registrations of EVERY annotated
// package any test file links — booting one module in isolation then fails on
// the other packages' providers. Scope the boot instead:
//
//	nexus.InProcess(config.Runtime{},
//	    nexus.DecoratedModules("adverts"),   // only adverts' //nexus: registrations
//	    adverts.Module, ...)
//
// With no names, every decorated registration is dropped — a boot fully
// isolated from annotations. Decorated modules are named after their package
// (the main package registers as "app"). The option is read from the boot's
// top-level option list only.
func DecoratedModules(names ...string) Option {
	return decoratedModulesOption{names: names}
}

type decoratedModulesOption struct{ names []string }

func (d decoratedModulesOption) nexusOption() di.Option { return di.Options() }

// filterDeferredOptions applies any DecoratedModules markers among the boot's
// own options to the drained deferred registrations. No marker → drained
// passes through untouched.
func filterDeferredOptions(userOpts, drained []Option) []Option {
	var keep map[string]bool
	for _, o := range userOpts {
		if d, ok := o.(decoratedModulesOption); ok {
			if keep == nil {
				keep = map[string]bool{}
			}
			for _, n := range d.names {
				keep[n] = true
			}
		}
	}
	if keep == nil {
		return drained
	}
	var out []Option
	for _, o := range drained {
		if m, ok := o.(moduleOption); ok && keep[m.name] {
			out = append(out, o)
		}
	}
	return out
}

// BootCheck is a boot-time self-check: it inspects live app topology after
// wiring and returns any foot-guns as manifest.Issues. It exists to promote
// failures that would otherwise only surface at runtime — e.g. pubsub's "topic
// has no transport bound", which today fails at the first Publish (possibly at
// 2am) — into a loud, early report at boot in dev, so a junior sees the problem
// before deploy. See ERRORS.md.
type BootCheck func() []manifest.Issue

var (
	bootChecks        []BootCheck
	pendingBootIssues []manifest.Issue // config-file lint issues collected during load
)

// RegisterBootCheck adds a boot-time self-check. Call it from an init() in the
// package that owns the invariant — the same pay-for-what-you-use pattern as
// dashboard.RegisterSnapshotExtra: the check only exists if the app imports
// that package, so the framework core stays free of the dependency. Safe to
// call before Run.
func RegisterBootCheck(fn BootCheck) {
	if fn != nil {
		bootChecks = append(bootChecks, fn)
	}
}

// addPendingBootIssues stashes issues discovered before the DI graph exists
// (e.g. the nexus.toml lint in autoLoad) so runBootChecks can report them
// alongside the topology checks in one block.
func addPendingBootIssues(issues []manifest.Issue) {
	pendingBootIssues = append(pendingBootIssues, issues...)
}

// collectBootIssues gathers the config-file issues plus every registered
// topology check's findings. Pure and drainable — separated from reporting so
// tests can assert on the issues without capturing stderr.
func collectBootIssues() []manifest.Issue {
	issues := append([]manifest.Issue(nil), pendingBootIssues...)
	for _, c := range bootChecks {
		issues = append(issues, c()...)
	}
	pendingBootIssues = nil // drain so a second Run in-process doesn't double-report
	return issues
}

// runBootChecks collects and reports boot self-check issues. Called from a DI
// invoke that Run appends LAST (so it runs after pubsub's BindTopics and every
// other wiring invoke) and only in dev — so production pays nothing and a clean
// app prints nothing. Advisory: it never aborts boot (genuine fatal misconfig
// already fails elsewhere); the value is EARLY VISIBILITY.
func runBootChecks() { reportBootIssues(collectBootIssues()) }

// reportBootIssues prints issues to stderr, errors first, deep-linkable by path.
func reportBootIssues(issues []manifest.Issue) {
	if len(issues) == 0 {
		return
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Severity != issues[j].Severity {
			return issues[i].Severity == manifest.SeverityError // errors before warnings
		}
		return issues[i].Path < issues[j].Path
	})
	fmt.Fprintf(os.Stderr,
		"\nnexus: boot self-check found %d issue(s) — dev-only, fix before deploy (see ERRORS.md):\n",
		len(issues))
	for _, is := range issues {
		fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", is.Severity, is.Path, is.Message)
	}
	fmt.Fprintln(os.Stderr)
}

// Environment / mode gating for the option chain. These helpers
// let an app's main() declare options that should ONLY apply in
// dev (or NEVER in dev) without growing a conditional on every
// option line.
//
// Canonical example: TLS / OAuth2 / config.Client are wired in
// production but skipped under `nexus dev` so the framework's
// dev mode (which sets NEXUS_DEV=1) boots without operator-
// supplied secrets / certs / config-server endpoints. Wrapping
// the real options:
//
//	nexus.Run(config.Runtime{...},
//	    nexus.IfNotDev(
//	        tls.Module(tls.Config{Domains: []string{"app.example.com"}}),
//	        oauth2.Module(...),
//	        config.Client("https://configd.internal:7100", ...),
//	    ),
//	    nexus.IfDev(
//	        config.Local("nexus.config.toml"),
//	    ),
//	    appModule,
//	)
//
// keeps production startup strict (TLS needs a domain) and dev
// startup fast (everything skipped, local config substituted).

// IfDev applies the supplied options ONLY when running under
// `nexus dev` (NEXUS_DEV=1). In production the wrapped block is
// a no-op — useful for dev-only stubs (config.Local, in-memory
// auth, fake mailers) that have no place in a real deploy.
//
// Variadic so multiple options compose cleanly without a
// surrounding nexus.Options(...) call. Empty input is a no-op
// regardless of mode.
func IfDev(opts ...Option) Option {
	if !dev.Enabled() {
		return Options() // no-op
	}
	return Options(opts...)
}

// IfNotDev applies the supplied options ONLY when NOT running
// under `nexus dev`. The mirror image of IfDev — gate plugins
// that require production-grade configuration (TLS certs,
// signing keys, OAuth2 client secrets, config-server URLs) so
// `nexus dev` boots without forcing the operator to fill those
// in.
//
//	nexus.IfNotDev(
//	    tls.Module(tls.Config{Domains: []string{"app.example.com"}}),
//	    oauth2.Module(oauth2.Config{ClientSecret: secret}),
//	)
//
// `nexus dev` skips both; `./bin/app` wires them normally.
func IfNotDev(opts ...Option) Option {
	if dev.Enabled() {
		return Options() // no-op
	}
	return Options(opts...)
}

// Setup registers work that must finish before the app serves —
// migrations, roles, indexes, seeds, backfills:
//
//	nexus.Setup(EnsureReportRole, EnsureIndexes)
//
// Each fn's parameters come from DI like a provider's, except a
// context.Context, which receives the boot context; it returns nothing or
// an error. Setup functions run after every resource and worker has
// started and before the listeners open, in declaration order; the first
// error stops boot, naming the function. They are listed in the deployment
// manifest as pre-start tasks.
func Setup(fns ...any) Option {
	opts := make([]Option, 0, len(fns))
	for _, fn := range fns {
		opts = append(opts, setupTask(fn))
	}
	return Options(opts...)
}

func setupTask(fn any) Option {
	v := reflect.ValueOf(fn)
	t := v.Type()
	if t.Kind() != reflect.Func || t.IsVariadic() || t.NumOut() > 1 || (t.NumOut() == 1 && t.Out(0) != errorType) {
		return rawOption{di.Error(fmt.Errorf("nexus.Setup: %T must be a func(deps…) error", fn))}
	}
	name := funcDisplayName(v)
	// The invoke resolves the DI parameters now; the task runs them at
	// boot, with the context in whichever slot asks for one.
	in := []reflect.Type{appPtrType}
	var ctxAt []int
	for i := 0; i < t.NumIn(); i++ {
		if t.In(i) == contextType {
			ctxAt = append(ctxAt, i)
			continue
		}
		in = append(in, t.In(i))
	}
	invoke := reflect.MakeFunc(reflect.FuncOf(in, nil, false), func(args []reflect.Value) []reflect.Value {
		deps := args[1:]
		args[0].Interface().(*App).AddStartupTask(manifest.StartupTask{
			Name: name,
			Run: func(ctx context.Context) error {
				call := make([]reflect.Value, 0, t.NumIn())
				d := 0
				for i := 0; i < t.NumIn(); i++ {
					if slices.Contains(ctxAt, i) {
						call = append(call, reflect.ValueOf(ctx))
						continue
					}
					call = append(call, deps[d])
					d++
				}
				out := v.Call(call)
				if len(out) == 1 && !out[0].IsNil() {
					return out[0].Interface().(error)
				}
				return nil
			},
		})
		return nil
	})
	return Invoke(invoke.Interface())
}

// funcDisplayName is a function's name as a reader knows it: the package
// path dropped, a method's receiver kept (users.(*Store).Seed → (*Store).Seed).
func funcDisplayName(v reflect.Value) string {
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		return v.Type().String()
	}
	name := f.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, "-fm")
}
