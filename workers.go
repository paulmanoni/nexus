package nexus

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/registry"
)

// AsWorker registers a long-lived background worker. The framework
// owns lifecycle — it starts the worker on fx.Start in its own
// goroutine, cancels its context on fx.Stop, and records status +
// last-error on the registry so the dashboard can surface it.
//
// Signature requirements:
//   - First parameter MUST be context.Context. The framework supplies
//     a context that cancels when the app stops; workers are expected
//     to honor it and return.
//   - Remaining parameters are fx-injected deps (same rules as a
//     constructor — they must exist in the graph).
//   - Return is optional: a single (error) return lets the worker
//     report a fatal error. context.Canceled / nil is treated as a
//     clean stop; anything else sets Status="failed" + LastError.
//
//	nexus.AsWorker("cache-invalidation",
//	    func(ctx context.Context, db *OatsDB, cache *CacheManager, logger *zap.Logger) error {
//	        for !db.IsConnected() {
//	            select {
//	            case <-ctx.Done(): return ctx.Err()
//	            case <-time.After(time.Second):
//	            }
//	        }
//	        listener := pq.NewListener(db.ConnectionString(), ...)
//	        defer listener.Close()
//	        _ = listener.Listen("cache_invalidation")
//	        for {
//	            select {
//	            case <-ctx.Done(): return nil
//	            case n := <-listener.Notify: handle(n, cache)
//	            }
//	        }
//	    })
//
// Resource / service deps (for the architecture graph) are detected
// the same way nexus.ProvideService does it — any param implementing
// NexusResourceProvider contributes its resources, any param whose
// type is a service wrapper contributes a service dep.
//
// A worker panic is caught and reported as Status=failed; the app
// keeps running. For restart semantics, wrap your worker body in a
// loop that re-dials on ctx.Done() exit OR let the operator restart
// the app.
func AsWorker(name string, fn any) Option {
	if name == "" {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWorker requires a non-empty name"))}
	}
	rt := reflect.TypeOf(fn)
	if rt == nil || rt.Kind() != reflect.Func {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWorker fn must be a function"))}
	}
	if rt.NumIn() < 1 {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWorker %q fn's first param must be context.Context", name))}
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	if rt.In(0) != ctxType {
		return rawOption{o: fx.Error(fmt.Errorf("nexus: AsWorker %q fn's first param must be context.Context (got %s)", name, rt.In(0)))}
	}

	// Invoke signature: (*App, fx.Lifecycle, ...depParams) — we strip
	// ctx because fx injects the REMAINING deps at invoke time, and
	// we fill ctx at run time with our managed lifecycle context.
	appType := reflect.TypeOf((*App)(nil))
	lcType := reflect.TypeOf((*fx.Lifecycle)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()
	in := make([]reflect.Type, 0, rt.NumIn()+1)
	in = append(in, appType, lcType)
	for i := 1; i < rt.NumIn(); i++ {
		in = append(in, rt.In(i))
	}
	invokeType := reflect.FuncOf(in, nil, false)

	invokeFn := reflect.MakeFunc(invokeType, func(args []reflect.Value) []reflect.Value {
		app := args[0].Interface().(*App)
		lc := args[1].Interface().(fx.Lifecycle)
		deps := args[2:]

		// Detect resource / service deps from the typed params, same
		// convention as ProvideService. Duplicates de-duped by the
		// registry's dedupeSort in Set*.
		var resourceDeps []string
		var serviceDeps []string
		for i := 0; i < len(deps); i++ {
			depType := rt.In(i + 1) // +1 because we stripped ctx
			depVal := deps[i]
			if !depVal.IsValid() {
				continue
			}
			if provider, ok := depVal.Interface().(NexusResourceProvider); ok {
				for _, r := range provider.NexusResources() {
					resourceDeps = append(resourceDeps, r.Name())
				}
			}
			if isServiceWrapperType(depType) {
				if svc, ok := unwrapService(depVal, depType); ok && svc != nil {
					serviceDeps = append(serviceDeps, svc.Name())
				}
			}
		}
		app.Registry().RegisterWorker(registry.Worker{
			Name:         name,
			Status:       "starting",
			ResourceDeps: resourceDeps,
			ServiceDeps:  serviceDeps,
		})

		ctx, cancel := context.WithCancel(context.Background())
		lc.Append(fx.Hook{
			OnStart: func(_ context.Context) error {
				go runWorker(name, fn, ctx, deps, errType, app)
				return nil
			},
			OnStop: func(_ context.Context) error {
				cancel()
				return nil
			},
		})
		return nil
	})
	return rawOption{o: fx.Invoke(invokeFn.Interface())}
}

// runWorker invokes the user's function reflectively on a dedicated
// goroutine, recovering from panics and recording final status.
// Kept separate from the Invoke closure so the recover/defer chain
// is readable and the hot path stays out of an already-deep
// reflect.MakeFunc body.
func runWorker(name string, fn any, ctx context.Context, deps []reflect.Value, errType reflect.Type, app *App) {
	app.Registry().UpdateWorkerStatus(name, "running", "")
	defer func() {
		if r := recover(); r != nil {
			app.Registry().UpdateWorkerStatus(name, "failed", fmt.Sprintf("panic: %v", r))
		}
	}()

	callIn := make([]reflect.Value, 0, len(deps)+1)
	callIn = append(callIn, reflect.ValueOf(ctx))
	callIn = append(callIn, deps...)
	out := reflect.ValueOf(fn).Call(callIn)

	// Error return is optional; scan for the first (error) slot.
	var runErr error
	for _, o := range out {
		if o.Type().Implements(errType) && !o.IsNil() {
			runErr = o.Interface().(error)
			break
		}
	}
	status := "stopped"
	msg := ""
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		status = "failed"
		msg = runErr.Error()
	}
	app.Registry().UpdateWorkerStatus(name, status, msg)
}