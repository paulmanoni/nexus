package nexus

import (
	"fmt"
	"reflect"

	"go.uber.org/fx"
)

// ComponentSpec carries everything AsComponent collects from its
// caller — name, constructor, URL mount path, template source
// path — before handing off to a registered LiveAdapter. Exported
// because option types defined in subpackages (template.WithTemplate
// and friends) populate these fields directly from outside the
// nexus package.
type ComponentSpec struct {
	// Name is the engine-side component identifier. Used as the
	// key in the engine's component registry and as the value
	// referenced by <ComponentName /> tags in templates.
	Name string

	// Ctor is the per-session constructor — its signature is
	// inspected by AsComponent. Parameters are resolved from the
	// fx graph; the return is (T) or (T, error) where T must
	// satisfy whatever Component interface the adapter expects
	// (template.Component for the built-in live template engine).
	Ctor any

	// URLPath, when non-empty, mounts the component's HTTP
	// handler at that path on the app's gin engine. Empty means
	// "child component" — registered with the engine but not
	// reachable as a top-level page.
	URLPath string

	// TemplatePath is the source location passed by
	// template.WithTemplate. The adapter interprets it (e.g.,
	// resolves against the embed.FS supplied to template.Module).
	TemplatePath string
}

// ComponentOption mutates a ComponentSpec. nexus.Path lives here;
// renderer-specific options like template.WithTemplate live in
// their renderer's package.
type ComponentOption interface{ Apply(*ComponentSpec) }

type componentOptFunc func(*ComponentSpec)

func (f componentOptFunc) Apply(s *ComponentSpec) { f(s) }

// LiveAdapter is the seam between AsComponent (which knows about
// constructors and options) and a concrete renderer (which knows
// how to load templates, build per-session instances, and mount
// HTTP handlers). live/template registers an implementation in
// its fx Module so AsComponent finds an adapter when it builds
// its fx.Invoke.
//
// factory has type func() any rather than func() template.Component
// because nexus must not import live/template; the adapter casts
// the concrete result back to its own Component interface.
type LiveAdapter interface {
	RegisterComponent(spec *ComponentSpec, factory func() any) error
}

// AsComponent registers a live component on whatever LiveAdapter
// the graph provides. The constructor's parameters are resolved
// from the fx container; the constructor is called fresh on every
// session start (so per-session state lives on the returned
// struct) while its captured deps are singletons.
//
// Usage:
//
//	nexus.AsComponent("Posts",
//	    func(repo *PostsRepo) (*PostsList, error) {
//	        return &PostsList{repo: repo}, nil
//	    },
//	    template.WithTemplate("templates/posts"),
//	    nexus.Path("/"),
//	)
//
// The constructor signature is arbitrary. Returns must be (T) or
// (T, error). T is what gets passed to the adapter; the adapter
// is responsible for any further type assertion (e.g., that T
// implements template.Component).
//
// Implementation: a dynamic fx.Invoke is synthesized via
// reflect.MakeFunc — its parameter list is (LiveAdapter,
// ctor-params...), which lets fx resolve everything in one shot.
// The synthesized body captures the ctor-params in a closure and
// hands the adapter a per-session factory that calls the
// constructor on each invocation.
func AsComponent(name string, ctor any, opts ...ComponentOption) Option {
	spec := &ComponentSpec{Name: name, Ctor: ctor}
	for _, o := range opts {
		o.Apply(spec)
	}

	ctorType := reflect.TypeOf(ctor)
	if ctorType == nil || ctorType.Kind() != reflect.Func {
		return Raw(fx.Error(fmt.Errorf(
			"nexus: AsComponent %q: ctor must be a function, got %T", name, ctor,
		)))
	}
	if ctorType.NumOut() == 0 || ctorType.NumOut() > 2 {
		return Raw(fx.Error(fmt.Errorf(
			"nexus: AsComponent %q: ctor must return (T) or (T, error), got %d return values",
			name, ctorType.NumOut(),
		)))
	}
	if ctorType.NumOut() == 2 {
		errIface := reflect.TypeOf((*error)(nil)).Elem()
		if !ctorType.Out(1).Implements(errIface) {
			return Raw(fx.Error(fmt.Errorf(
				"nexus: AsComponent %q: second ctor return must be error, got %s",
				name, ctorType.Out(1),
			)))
		}
	}

	adapterIface := reflect.TypeOf((*LiveAdapter)(nil)).Elem()
	errType := reflect.TypeOf((*error)(nil)).Elem()

	paramTypes := []reflect.Type{adapterIface}
	for i := 0; i < ctorType.NumIn(); i++ {
		paramTypes = append(paramTypes, ctorType.In(i))
	}
	fnType := reflect.FuncOf(paramTypes, []reflect.Type{errType}, false)
	ctorVal := reflect.ValueOf(ctor)

	fn := reflect.MakeFunc(fnType, func(args []reflect.Value) []reflect.Value {
		adapter := args[0].Interface().(LiveAdapter)
		ctorArgs := args[1:]
		factory := func() any {
			results := ctorVal.Call(ctorArgs)
			// (T, error) shape: panic on non-nil error so the
			// failing session join surfaces visibly — we can't
			// return errors from a factory called per-WS-connection.
			if len(results) == 2 && !results[1].IsNil() {
				panic(fmt.Errorf("nexus: component %q ctor: %w", spec.Name, results[1].Interface().(error)))
			}
			return results[0].Interface()
		}
		if err := adapter.RegisterComponent(spec, factory); err != nil {
			return []reflect.Value{reflect.ValueOf(err)}
		}
		return []reflect.Value{reflect.Zero(errType)}
	})
	return Raw(fx.Invoke(fn.Interface()))
}
