package nexus

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
