package nexus

import (
	"os"
	"reflect"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

// Option composes a nexus app. Everything returned by Provide, Supply,
// Invoke, Module, AsRest, AsQuery, AsMutation, AsWebSocket, AsSubscription
// is an Option, ready to pass to Run. fx is an implementation detail —
// user code imports only nexus.
type Option interface{ nexusOption() fx.Option }

type rawOption struct{ o fx.Option }

func (r rawOption) nexusOption() fx.Option { return r.o }

// Module groups options under a name. Mirrors fx.Module's logging — the
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
	// only needs to land before fx.Start fires constructors —
	// which happens after this whole Module() call returns.
	if publicPath != "" {
		registerModulePublicPath(name, publicPath)
	}

	// Stamp module name + route prefix onto every child option that
	// cares. Options produced by nested Module(...) don't implement
	// these annotator interfaces (they return a rawOption wrapping
	// fx.Module), so inner-most wins automatically — the inner
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
	return rawOption{o: fx.Module(name, unwrap(opts)...)}
}

// Options bundles multiple Option values into a single Option.
// Useful when one logical feature expands into several: a
// conditional gate that pulls in a frontend mount + a config
// supply + an extra invoke, for example. Empty input is a no-op.
func Options(opts ...Option) Option {
	if len(opts) == 0 {
		return rawOption{o: fx.Options()}
	}
	return rawOption{o: fx.Options(unwrap(opts)...)}
}

// activeDeploymentMatches always returns false — the deployment-split
// feature has been removed. Kept as an unexported stub so the rest of
// this file builds without further surgery; no caller uses it.
func activeDeploymentMatches(names []string) bool {
	return false
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
// fx.Provide — return types enter the graph, params resolve from it.
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
	opts := make([]fx.Option, 0, len(fns)+1)
	opts = append(opts, fx.Provide(fns...))
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
	return rawOption{o: fx.Options(opts...)}
}

// Supply puts concrete values into the graph (no constructor). Useful for
// config structs or pre-built instances created outside the fx graph.
//
//	nexus.Supply(nexus.Config{Server: ServerConfig{Addr: ":8080"}})   // rare — Run takes Config directly
//	nexus.Supply(myAlreadyBuiltClient)          // typical
func Supply(values ...any) Option {
	return rawOption{o: fx.Supply(values...)}
}

// Invoke runs a function at startup, resolving its parameters from the
// graph. Use for side-effects on boot — attaching resources, registering
// hooks, seeding state. Multiple Invoke options run in registration order.
//
//	nexus.Invoke(func(app *nexus.App, dbs *DBManager) {
//	    app.OnResourceUse(dbs)
//	})
func Invoke(fns ...any) Option {
	return rawOption{o: fx.Invoke(fns...)}
}

// serviceDepsRegisterInvoke synthesizes an fx.Invoke that takes the
// constructed service + ALL of the constructor's original params,
// walks them for NexusResourceProvider / service-wrapper values, and
// calls registry.SetServiceDeps with the resulting name lists.
// Returns nil when fn isn't a function or its return isn't a
// service wrapper — letting ProvideService degrade to a plain
// Provide without failing boot.
func serviceDepsRegisterInvoke(fn any) fx.Option {
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
	return fx.Invoke(invokeFn.Interface())
}

// resourceAutoRegisterInvoke synthesizes an fx.Invoke(func(app *App, instance T))
// that, at boot, registers resources and wires OnResourceUse for the instance.
// Returns nil when fn isn't a function, returns nothing, or its first
// return type doesn't implement NexusResourceProvider or UseReporter —
// skipping the invoke avoids forcing a *App dep on the graph for plain
// types (a regression that surfaces when nexus.Provide is used for
// unrelated values like func() string in tests).
func resourceAutoRegisterInvoke(fn any) fx.Option {
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
	return fx.Invoke(invokeFn.Interface())
}

// Raw is an escape hatch: accept any fx.Option and route it through nexus.
// For features nexus hasn't mirrored yet (fx.Decorate, fx.Replace, named
// values via fx.Annotate with ParamTags, etc.) or one-off integrations.
// Normal apps never need it.
//
//	nexus.Raw(fx.Decorate(wrapLogger))
func Raw(opt fx.Option) Option {
	return rawOption{o: opt}
}

// Run builds and runs the app. Blocks until SIGINT/SIGTERM, then
// gracefully shuts the HTTP server + cron scheduler. Returns nothing —
// identical to fx.App.Run(). For tests where you need explicit Start/Stop
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
	all := append([]fx.Option{
		fxEarlyOptions(cfg),
		autoManifestOptions(),
	}, unwrap(opts)...)
	all = append(all, fxLateOptions())
	switch {
	case os.Getenv("NEXUS_FX_QUIET") == "1":
		// Explicit user opt-out: silence everything including errors.
		// They asked for it.
		all = append(all, fx.NopLogger)
	case devQuiet:
		// Implicit dev quiet (NEXUS_DEV=1, set by `nexus dev` without
		// --verbose). Drop the boot chatter but keep error events —
		// otherwise an fx constructor / Invoke / lifecycle failure
		// exits the binary silently and `nexus dev` shows
		// "app exited: exit status 1" with nothing above it.
		all = append(all, fx.WithLogger(func() fxevent.Logger {
			return &fxErrorOnlyLogger{inner: &fxevent.ConsoleLogger{W: os.Stderr}}
		}))
	}
	fx.New(all...).Run()
}


// fxErrorOnlyLogger wraps an fxevent.Logger and only forwards events
// that carry a non-nil error. Used in dev-quiet mode so fx graph
// failures (missing dependency, constructor returning error, lifecycle
// hook returning error) still reach stderr while the routine
// Provided/Invoked/Started chatter stays suppressed. Replaces
// fx.NopLogger on the implicit-quiet path — NopLogger ate the cause
// of "app exited: exit status 1" silent failures.
type fxErrorOnlyLogger struct{ inner fxevent.Logger }

func (l *fxErrorOnlyLogger) LogEvent(event fxevent.Event) {
	if fxEventErr(event) != nil {
		l.inner.LogEvent(event)
	}
}

// fxEventErr extracts the Err field from any fxevent type that carries
// one. Events without an error field always return nil so they're
// filtered out. RollingBack reports StartErr (the cause that triggered
// the rollback) since that's the actionable signal.
func fxEventErr(event fxevent.Event) error {
	switch e := event.(type) {
	case *fxevent.OnStartExecuted:
		return e.Err
	case *fxevent.OnStopExecuted:
		return e.Err
	case *fxevent.Supplied:
		return e.Err
	case *fxevent.Provided:
		return e.Err
	case *fxevent.Replaced:
		return e.Err
	case *fxevent.Decorated:
		return e.Err
	case *fxevent.Run:
		return e.Err
	case *fxevent.Invoked:
		return e.Err
	case *fxevent.Started:
		return e.Err
	case *fxevent.Stopped:
		return e.Err
	case *fxevent.RollingBack:
		return e.StartErr
	case *fxevent.RolledBack:
		return e.Err
	case *fxevent.LoggerInitialized:
		return e.Err
	}
	return nil
}

// unwrap flattens a []Option into the []fx.Option fx needs internally.
func unwrap(opts []Option) []fx.Option {
	out := make([]fx.Option, len(opts))
	for i, o := range opts {
		out[i] = o.nexusOption()
	}
	return out
}
