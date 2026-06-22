package nexus

import (
	"errors"
	"io/fs"
	"os"
	"reflect"

	"github.com/paulmanoni/nexus/di"
	"github.com/paulmanoni/nexus/httpx"
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
//	var advertsModule = nexus.Module("adverts",
//	    nexus.Provide(NewAdvertsService),
//	    nexus.AsQuery(NewGetAllAdverts),
//	    nexus.AsMutation(NewCreateAdvert, …),
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
		if prefix != "" {
			if rp, ok := o.(restPrefixAnnotator); ok {
				rp.setRestPrefix(prefix)
			}
		}
	}
	return rawOption{o: di.Module(name, unwrap(opts)...)}
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
//	    NewAdvertsService,   // service wrapper — deps recorded
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
//	nexus.Supply(nexus.Config{Server: ServerConfig{Addr: ":8080"}})   // rare — Run takes Config directly
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
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func || rt.NumOut() == 0 {
		return nil
	}
	// First return is the constructed instance. Ignore trailing error return.
	outType := rt.Out(0)
	providerIface := reflect.TypeOf((*NexusResourceProvider)(nil)).Elem()
	reporterIface := reflect.TypeOf((*UseReporter)(nil)).Elem()
	if !outType.Implements(providerIface) && !outType.Implements(reporterIface) {
		return nil
	}

	invokeType := reflect.FuncOf(
		[]reflect.Type{reflect.TypeOf((*App)(nil)), outType},
		nil, false,
	)
	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		inst := args[1].Interface()
		if p, ok := inst.(NexusResourceProvider); ok {
			for _, r := range p.NexusResources() {
				app.Register(r)
			}
		}
		if reporter, ok := inst.(UseReporter); ok {
			app.OnResourceUse(reporter)
		}
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

// Run builds and runs the app. Blocks until SIGINT/SIGTERM, then
// gracefully shuts the HTTP server + cron scheduler. Returns nothing —
// identical to di.App.Run(). For tests where you need explicit Start/Stop
// control, build the app via a test helper that calls fxBootOptions.
//
//	func main() {
//	    nexus.Run(
//	        nexus.Config{Addr: ":8080", EnableDashboard: true},
//	        nexus.Provide(NewDBManager),
//	        advertsModule,
//	    )
//	}
//
// When NEXUS_FX_QUIET=1 is set in the environment, fx's startup log
// (PROVIDE/INVOKE/HOOK lines) is suppressed. The splitter sets this
// in subprocesses so the prefixed log streams don't drown in fx
// scaffolding noise; users hitting framework-level issues can unset
// it for full diagnostics.
// Boot loads nexus.toml automatically — the [runtime] Config, every
// [extensions.*] block, the [env] bridge, and the nexus.Get base
// layer — then runs the app. It's the zero-boilerplate form of:
//
//	cfg  := nexus.MustLoadConfig()
//	opts := nexus.MustLoadExtensions()
//	nexus.Run(cfg, append(opts, userOpts...)...)
//
// so main() collapses to:
//
//	func main() {
//	    nexus.Boot(
//	        nexus.ServeFrontend(webFS, "web/dist"),
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

// resolveConfigPath picks the nexus.toml path: the NEXUS_CONFIG env
// override when set, else the conventional DefaultConfigPath.
func resolveConfigPath() string {
	if p := os.Getenv("NEXUS_CONFIG"); p != "" {
		return p
	}
	return DefaultConfigPath
}

// autoLoad reads runtime Config + extension options from path. A
// missing file yields a zero Config and no extensions; any other
// load/parse error panics so a malformed config fails loudly at
// startup rather than silently dropping settings.
func autoLoad(path string) (Config, []Option) {
	cfg, err := LoadConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg = Config{}
		} else {
			panic(err)
		}
	}
	extOpts, err := LoadExtensionOptions(path)
	if err != nil {
		panic(err)
	}
	return cfg, extOpts
}

func Run(cfg Config, opts ...Option) {
	// Print-mode short-circuit. When NEXUS_PRINT_MANIFEST=1 is set,
	// the orchestration platform is invoking us at build/upload time
	// to extract the manifest. Build the fx graph, populate *App
	// (which fires every DeclareEnv / DeclareService / UseVolume /
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
	// Quiet-by-default in dev: nexus dev sets NEXUS_DEV=1 on the
	// child, which here implies "suppress [Fx] graph chatter and
	// [GIN-debug] route-registration spam unless the user wants
	// them back". The opt-out is NEXUS_VERBOSE=1 (set by the
	// `nexus dev --verbose` flag). Users running `go run` directly
	// keep today's behavior — neither env var is set.
	devQuiet := os.Getenv("NEXUS_DEV") == "1" && os.Getenv("NEXUS_VERBOSE") != "1"
	if devQuiet {
		// Don't override an explicit GIN_MODE — operators sometimes
		// pin it for reasons we can't see (CI, container images).
		if os.Getenv("GIN_MODE") == "" {
			_ = os.Setenv("GIN_MODE", "release")
		}
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
	// Deferred sources (e.g. nexus/decorate's //@-annotation drain) contribute
	// AFTER the app's own options and BEFORE autoMountGraphQL, so their
	// endpoints take part in schema assembly like any hand-written module.
	all = append(all, unwrap(collectDeferredOptions())...)
	all = append(all, fxLateOptions())
	// The builtin container prints build/start errors to stderr itself; the
	// opt-in fx adapter owns its own logging (and honors NEXUS_FX_QUIET).
	// devQuiet only governs the gin route-registration spam, set above.
	_ = devQuiet
	backend.Build(di.Collect(all...)).Run()
}

// unwrap flattens a []Option into the []di.Option the container needs.
func unwrap(opts []Option) []di.Option {
	out := make([]di.Option, len(opts))
	for i, o := range opts {
		out[i] = o.nexusOption()
	}
	return out
}
