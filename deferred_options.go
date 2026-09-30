package nexus

import "github.com/paulmanoni/nexus/di"

// deferredOptionSources are functions that yield Options at Boot/Run time —
// AFTER every package init() has run. This is the seam that lets nexus/decorate
// auto-wire //@-annotated registrations without the app writing an explicit
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

// DecoratedModules limits which //@-annotated (decorate-registered) modules
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
//	nexus.InProcess(nexus.Config{},
//	    nexus.DecoratedModules("adverts"),   // only adverts' //@ registrations
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
