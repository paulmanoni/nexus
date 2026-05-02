package nexus

import (
	"fmt"
	"os"
	"reflect"

	"go.uber.org/fx"
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
	// DeployAs is at-most-once per Module; last write wins. Empty
	// string keeps the module untagged (always-local).
	var deployment string
	var explicitDeploy bool
	for _, o := range opts {
		if rp, ok := o.(routePrefixOption); ok {
			prefix += rp.prefix
		}
		if pp, ok := o.(pathOption); ok {
			publicPath = pp.path
			prefix += pp.path
		}
		if dt, ok := o.(deployTagOption); ok {
			deployment = dt.tag
			explicitDeploy = true
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

	// Manifest fallback: if the user didn't write nexus.DeployAs(...)
	// in source, consult the inferred-tag registry populated from
	// nexus.deploy.yaml's `deployments[X].owns: [name]` entries by
	// the codegen'd init() in zz_deploy_gen.go. Lets the manifest be
	// the single source of truth for deployment tagging when the
	// developer prefers manifest-driven config; explicit DeployAs
	// still wins above when present.
	if !explicitDeploy {
		if t := inferredDeployTag(name); t != "" {
			deployment = t
		}
	}

	// Split-mode filter: when NEXUS_DEPLOYMENT names a tag, modules
	// declaring a different DeployAs(...) are skipped wholesale —
	// their Provide/AsRest/AsQuery/AsWS calls never reach the fx
	// graph, so duplicate-provider errors disappear and the
	// in-process service stays scoped to the active deployment.
	// Untagged modules (libraries, shared middleware) are always
	// local and run in every deployment. Cross-module callers
	// reach skipped peers through the codegen'd remote client,
	// which is registered separately.
	if active := os.Getenv(nexusDeploymentEnv); active != "" && deployment != "" && deployment != active {
		// Use an empty fx.Module so the name still shows up in
		// fx's startup logs (helps the user confirm the filter
		// fired) but no providers run.
		return rawOption{o: fx.Module(name + " (skipped: " + deployment + ")")}
	}

	// Stamp module name + route prefix + deployment tag onto every
	// child option that cares. Options produced by nested Module(...)
	// don't implement these annotator interfaces (they return a
	// rawOption wrapping fx.Module), so inner-most wins automatically
	// — the inner Module() already annotated its own children before
	// we see it.
	for _, o := range opts {
		if ma, ok := o.(moduleAnnotator); ok {
			ma.setModule(name)
		}
		if prefix != "" {
			if rp, ok := o.(restPrefixAnnotator); ok {
				rp.setRestPrefix(prefix)
			}
		}
		if deployment != "" {
			if da, ok := o.(deploymentAnnotator); ok {
				da.setDeployment(deployment)
			}
		}
	}
	return rawOption{o: fx.Module(name, unwrap(opts)...)}
}

// nexusDeploymentEnv is the env var the splitter sets per subprocess.
// Mirrored from DeploymentFromEnv() in config.go so options.go can
// consult it without an import cycle.
const nexusDeploymentEnv = "NEXUS_DEPLOYMENT"

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

// IfDeployment yields opts as a single Option only when the
// active deployment matches one of names. When it doesn't match,
// the returned Option is a no-op so all listed opts (and any
// Provide / Invoke / Module they wrap) are skipped — no
// constructors run, no routes register, no fx graph touched.
//
// Active deployment, in priority:
//
//  1. NEXUS_DEPLOYMENT env var (set per subprocess by
//     `nexus dev --split`)
//  2. DeploymentDefaults.Deployment (baked into the binary by
//     `nexus build --deployment X` for any deployment whose
//     manifest entry has an explicit `owns:` key — listed or
//     empty []. Omitted owns: monolith leaves this empty.)
//  3. Empty → matches "monolith" by convention so a plain
//     `go run .` against an unannotated build still hits
//     monolith-only opts.
//
// The classic use is gating a frontend mount on the
// monolith / web-svc deployments while leaving uaa-svc /
// interview-svc as API-only binaries that ship no SPA bytes:
//
//	nexus.Run(nexus.Config{...},
//	    nexus.IfDeployment([]string{"monolith", "web-svc"},
//	        nexus.ServeFrontend(distFS, "web/dist"),
//	    ),
//	    uaa.Module,
//	    interview.Module,
//	)
//
// Manifest pairing for the web-svc shape:
//
//	deployments:
//	  monolith:                  # owns: omitted → owns everything
//	    port: 8080
//	  web-svc:
//	    owns: []                 # explicit empty → owns nothing
//	    port: 9000               # tiny SPA-only binary
//	  uaa-svc:
//	    owns: [uaa]
//	    port: 9001
func IfDeployment(names []string, opts ...Option) Option {
	if !activeDeploymentMatches(names) {
		return rawOption{o: fx.Options()}
	}
	return Options(opts...)
}

// activeDeploymentMatches reports whether the active deployment
// (resolved via the priority chain above) is in names. Empty
// active falls back to "monolith" so single-process dev runs
// hit monolith-gated opts.
func activeDeploymentMatches(names []string) bool {
	active := os.Getenv(nexusDeploymentEnv)
	if active == "" {
		if d, ok := loadDeploymentDefaults(); ok {
			active = d.Deployment
		}
	}
	if active == "" {
		active = "monolith"
	}
	for _, n := range names {
		if n == active {
			return true
		}
	}
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
	// Apply the framework's precedence chain (manifest defaults →
	// env var fallback) so validateTopology and downstream New() see
	// the same resolved Config. Explicit fields always win.
	cfg = resolveConfig(cfg)
	if err := validateTopology(cfg); err != nil {
		// Boot-time misconfiguration — fail before fx spins up so the
		// operator sees a single clean line instead of an fx stack
		// trace. Mirrors how net.Listen errors surface today.
		panic(err)
	}
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
	// Two-phase split: fxEarlyOptions seeds Config + *App + lifecycle
	// BEFORE user opts run, then user opts (which may install global
	// middleware via auth.Module / engine.Use), then fxLateOptions
	// runs autoMountGraphQL last so GraphQL routes pick up every
	// user-installed middleware. Without the split, GraphQL routes
	// registered first wouldn't see middleware Use()'d afterwards
	// — gin captures middleware at route-registration time.
	all := append([]fx.Option{fxEarlyOptions(cfg), autoClientOptions()}, unwrap(opts)...)
	all = append(all, fxLateOptions())
	if os.Getenv("NEXUS_FX_QUIET") == "1" {
		all = append(all, fx.NopLogger)
	}
	fx.New(all...).Run()
}

// validateTopology cross-checks Config.Deployment against
// Config.Topology.Peers when both are set. The active deployment must
// be a key in Peers — that's the operator's promise that they've
// declared the unit they're booting as. Empty Topology means "no peer
// table" which is fine in monolith and as a back-compat path; it
// short-circuits the check.
//
// Callers must pass a Config that's already been through
// resolveConfig — no env-var fallback happens here.
func validateTopology(cfg Config) error {
	if len(cfg.Topology.Peers) == 0 {
		return nil
	}
	deployment := cfg.Deployment
	if deployment == "" {
		// Monolith run with a populated Topology is permitted —
		// the table is unused but keeping it doesn't break anything.
		return nil
	}
	if _, ok := cfg.Topology.Peers[deployment]; !ok {
		keys := make([]string, 0, len(cfg.Topology.Peers))
		for k := range cfg.Topology.Peers {
			keys = append(keys, k)
		}
		return &UserError{
			Op:    "topology",
			Msg:   fmt.Sprintf("Deployment %q is not declared in Config.Topology.Peers", deployment),
			Notes: []string{fmt.Sprintf("declared peers: %v", keys)},
			Hint:  fmt.Sprintf(`add Topology.Peers[%q] in main.go's nexus.Config — URL may be empty for the active unit`, deployment),
		}
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
