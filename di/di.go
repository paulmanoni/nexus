// Package di is nexus's built-in dependency-injection container: a small,
// zero-third-party-dependency replacement for the subset of go.uber.org/fx
// that the framework actually uses.
//
// It is NOT a general-purpose clone of fx. It implements exactly the
// primitives nexus relies on — Provide / Supply / Invoke, optional and
// value-group dependencies (via In parameter objects or ParamTags/ResultTags
// annotations), ordered Lifecycle start/stop hooks, Populate, Module grouping,
// and option-time Error propagation — and nothing else. The goal is to let the
// default nexus binary link no fx/dig/multierr/atomic while keeping fx
// available as an opt-in adapter for graphs that want it (mirroring the httpx
// router seam).
//
// The recorded options form a Spec (Collect), which a Backend turns into a
// running app. The Builtin backend is this package's own container; the
// nexus/di/fxcontainer module is an alternative Backend that translates the
// same Spec onto fx. Because both backends consume the identical Spec, nexus
// code speaks only di — never fx.
//
// Semantics deliberately match fx so the translation is faithful:
//   - Constructors are lazy: a Provide'd ctor runs only when something resolves
//     its output type (directly, via a group, or as an Invoke param).
//   - Invokes run eagerly at New(), in registration order, after every Provide
//     is registered (Provide order is irrelevant; Invoke order is preserved).
//   - Lifecycle OnStart hooks run at Start() in append order; OnStop hooks run
//     at Stop() in reverse.
package di

import (
	"reflect"
	"strings"
)

// In is embedded in a struct used as a constructor/invoke parameter to mark it
// a "parameter object": each exported field is resolved from the container
// individually. Mirrors fx.In / dig.In. nexus itself prefers ParamTags on
// annotated invokes (see Annotate) so its internals stay marker-free and the
// fx adapter can translate them, but In is supported for completeness.
type In struct{}

// Out is embedded in a constructor's result struct to mark it a "result
// object": each exported field is produced into the container as if returned
// separately. A `group:"name"` tag produces the field into that value group.
// Mirrors fx.Out / dig.Out.
type Out struct{}

var (
	inMarkerType  = reflect.TypeFor[In]()
	outMarkerType = reflect.TypeFor[Out]()
	errorType     = reflect.TypeFor[error]()
	lifecycleType = reflect.TypeFor[Lifecycle]()
)

// embedsIn reports whether t is a struct that anonymously embeds di.In, and so
// should be treated as a parameter object rather than resolved as a single
// value. Only a direct (one-level) embed is recognized — which is all nexus
// uses and all fx documents as the supported shape.
func embedsIn(t reflect.Type) bool { return embedsMarker(t, inMarkerType) }

// embedsOut is the result-object counterpart to embedsIn.
func embedsOut(t reflect.Type) bool { return embedsMarker(t, outMarkerType) }

func embedsMarker(t reflect.Type, marker reflect.Type) bool {
	if t == nil || t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type == marker {
			return true
		}
	}
	return false
}

// Option composes a container. Provide, Supply, Invoke, Options, Module, Error,
// and Populate all return an Option. The fx implementation detail is hidden:
// nexus user code (and the nexus package itself, once migrated) speaks only di.
type Option interface{ applyOption(*Spec) }

// Spec is the flattened set of recorded options — the neutral plan that any
// Backend consumes. Provides/supplies are order-independent; Invokes preserve
// their tree order so nexus's early→user→late invoke sequence is honored
// exactly as under di.
type Spec struct {
	Provides []ProvideSpec
	Supplies []any
	Invokes  []InvokeSpec
	Errs     []error
}

// ProvideSpec is one constructor plus optional per-position group annotations
// attached via Annotate. Bare ctors have nil tag slices.
type ProvideSpec struct {
	Ctor       any
	ResultTags []string // per result index: `group:"name"` routes that result into a group
	ParamTags  []string // per param index: `group:"name"` / `optional:"true"`
}

// InvokeSpec is one invoke function plus optional per-parameter annotations.
type InvokeSpec struct {
	Fn        any
	ParamTags []string
}

// Collect flattens an option tree into a Spec. Backends (Builtin and
// fxcontainer) both start from this.
func Collect(opts ...Option) *Spec {
	s := &Spec{}
	for _, o := range opts {
		if o != nil {
			o.applyOption(s)
		}
	}
	return s
}

type provideOption struct{ specs []ProvideSpec }

func (o provideOption) applyOption(s *Spec) { s.Provides = append(s.Provides, o.specs...) }

type supplyOption struct{ vals []any }

func (o supplyOption) applyOption(s *Spec) { s.Supplies = append(s.Supplies, o.vals...) }

type invokeOption struct{ specs []InvokeSpec }

func (o invokeOption) applyOption(s *Spec) { s.Invokes = append(s.Invokes, o.specs...) }

type errorOption struct{ err error }

func (o errorOption) applyOption(s *Spec) {
	if o.err != nil {
		s.Errs = append(s.Errs, o.err)
	}
}

type optionsGroup struct{ opts []Option }

func (o optionsGroup) applyOption(s *Spec) {
	for _, opt := range o.opts {
		if opt != nil {
			opt.applyOption(s)
		}
	}
}

// Provide registers one or more constructors. Each is a func whose parameters
// are resolved from the container and whose results are added to it. A
// trailing error result aborts the build if non-nil. A constructor may be
// wrapped with Annotate(...) to tag its results into value groups.
func Provide(ctors ...any) Option {
	specs := make([]ProvideSpec, 0, len(ctors))
	for _, c := range ctors {
		if a, ok := c.(annotatedCtor); ok {
			specs = append(specs, ProvideSpec{Ctor: a.ctor, ResultTags: a.resultGroups, ParamTags: a.paramGroups})
			continue
		}
		specs = append(specs, ProvideSpec{Ctor: c})
	}
	return provideOption{specs: specs}
}

// Supply puts already-constructed values into the container keyed by their
// dynamic type. Nil values are skipped.
func Supply(vals ...any) Option { return supplyOption{vals: vals} }

// Invoke registers functions to run eagerly at New(), in order, after all
// constructors are registered. Parameters resolve from the container; a
// trailing error result aborts the build. A function may be wrapped with
// Annotate(.., ParamTags(..)) to pull a parameter from a value group or mark
// it optional.
func Invoke(fns ...any) Option {
	specs := make([]InvokeSpec, 0, len(fns))
	for _, fn := range fns {
		if a, ok := fn.(annotatedCtor); ok {
			specs = append(specs, InvokeSpec{Fn: a.ctor, ParamTags: a.paramGroups})
			continue
		}
		specs = append(specs, InvokeSpec{Fn: fn})
	}
	return invokeOption{specs: specs}
}

// Options bundles several Options into one. Empty input is a no-op.
func Options(opts ...Option) Option { return optionsGroup{opts: opts} }

// Module groups options under a name. nexus extracts module metadata from its
// own option wrappers before reaching the container, so here the name is
// retained only for diagnostics/logging parity; functionally it flattens like
// Options. (Kept as a distinct constructor so the nexus.Module → di.Module
// mapping stays one-to-one and a future build could scope names.)
func Module(name string, opts ...Option) Option {
	_ = name
	return optionsGroup{opts: opts}
}

// Error injects an error discovered while building options (e.g. a malformed
// handler registration). It surfaces from App.Err() / aborts Run, matching
// di.Error — letting option constructors report failures lazily instead of
// panicking at call time.
func Error(err error) Option { return errorOption{err: err} }

// parseTag pulls keys out of a struct-tag-style string such as
// `group:"x" optional:"true"`. Empty string for an absent key.
func parseTag(tag string) reflect.StructTag {
	return reflect.StructTag(strings.TrimSpace(tag))
}
