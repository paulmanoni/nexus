package nexus

import (
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
	// Stamp the module name onto every child registration option that
	// cares. Options produced by nested Module(...) don't implement
	// moduleAnnotator (they return a rawOption wrapping fx.Module), so
	// inner-most wins automatically — the inner Module() already
	// annotated its own children before we see it.
	for _, o := range opts {
		if ma, ok := o.(moduleAnnotator); ok {
			ma.setModule(name)
		}
	}
	return rawOption{o: fx.Module(name, unwrap(opts)...)}
}

// moduleAnnotator is implemented by options that participate in the
// nexus.Module grouping — specifically AsQuery/AsMutation/AsRest. The
// Module() function walks its direct children and calls setModule on
// each implementer so the registered endpoint knows its module.
type moduleAnnotator interface {
	setModule(name string)
}

// Provide registers one or more constructor functions with the dep graph.
// Return types are entered into the graph; parameter types are resolved
// from it. Same semantics as fx.Provide.
//
//	nexus.Provide(NewDBManager, NewCacheManager)
func Provide(fns ...any) Option {
	return rawOption{o: fx.Provide(fns...)}
}

// Supply puts concrete values into the graph (no constructor). Useful for
// config structs or pre-built instances created outside the fx graph.
//
//	nexus.Supply(nexus.Config{Addr: ":8080"})   // rare — Run takes Config directly
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

// ProvideResources is like Provide but also auto-registers each constructed
// instance's resources at boot. For any fn whose returned value implements
// NexusResourceProvider, every resource.Resource it reports is passed to
// app.Register; if it also satisfies UseReporter (e.g. *multi.Registry),
// app.OnResourceUse is wired automatically so resolver→resource edges
// appear on first UsingCtx call.
//
// This replaces the old pattern of a "resources" module full of
// resource.NewDatabase / NewCache calls — managers now describe their
// resources themselves via NexusResources() []resource.Resource, and
// a single ProvideResources does all the wiring.
//
//	nexus.ProvideResources(ProvideDBs, NewCacheManager)
//
// Types that don't implement either interface fall through to a plain
// Provide — so it's safe to pass mixed providers.
func ProvideResources(fns ...any) Option {
	opts := []fx.Option{fx.Provide(fns...)}
	for _, fn := range fns {
		if inv := resourceAutoRegisterInvoke(fn); inv != nil {
			opts = append(opts, inv)
		}
	}
	return rawOption{o: fx.Options(opts...)}
}

// resourceAutoRegisterInvoke synthesizes an fx.Invoke(func(app *App, instance T))
// that, at boot, registers resources and wires OnResourceUse for the instance.
// Returns nil when fn isn't a single-return constructor.
func resourceAutoRegisterInvoke(fn any) fx.Option {
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func || rt.NumOut() == 0 {
		return nil
	}
	// First return is the constructed instance. Ignore trailing error return.
	outType := rt.Out(0)

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
func Run(cfg Config, opts ...Option) {
	all := append([]fx.Option{fxBootOptions(cfg)}, unwrap(opts)...)
	fx.New(all...).Run()
}

// unwrap flattens a []Option into the []fx.Option fx needs internally.
func unwrap(opts []Option) []fx.Option {
	out := make([]fx.Option, len(opts))
	for i, o := range opts {
		out[i] = o.nexusOption()
	}
	return out
}
