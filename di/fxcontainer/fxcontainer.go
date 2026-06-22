// Package fxcontainer is an opt-in DI backend that runs a nexus app on
// go.uber.org/fx instead of the builtin zero-dependency container.
//
// It lives in its own Go module so importing it — and only then — pulls fx and
// dig back into the build. The default nexus binary links neither. Mirrors the
// httpx/ginrouter seam.
//
//	import "github.com/paulmanoni/nexus/di/fxcontainer"
//
//	nexus.Boot(nexus.WithContainer(fxcontainer.New()))
//
// It implements di.Backend by translating the neutral di.Spec — the same plan
// the builtin container consumes — into fx options: Provide/Supply/Invoke,
// value-group and optional annotations (ResultTags/ParamTags, whose raw tag
// strings are byte-for-byte what fx expects), and Error. The di.Lifecycle the
// framework depends on is bridged to fx.Lifecycle so resource start/stop hooks
// register on fx's lifecycle unchanged.
package fxcontainer

import (
	"github.com/paulmanoni/nexus/di"
	"go.uber.org/fx"
)

// New returns the fx-backed di.Backend.
func New() di.Backend { return backend{} }

type backend struct{}

// Build translates the collected Spec into an fx app. The returned *fx.App
// already satisfies di.Instance (Err/Start/Stop/Run).
func (backend) Build(spec *di.Spec) di.Instance {
	opts := make([]fx.Option, 0, len(spec.Provides)+len(spec.Invokes)+3)

	// Quiet by default, matching the builtin container (which logs nothing and
	// surfaces failures through Err()/Start()). Without this fx prints its full
	// PROVIDE/INVOKE/RUN graph trace to stderr on every boot. Errors are still
	// returned via *fx.App.Err()/Start(), so nothing actionable is lost.
	opts = append(opts, fx.NopLogger)

	// Bridge di.Lifecycle → fx.Lifecycle so any constructor/invoke that takes a
	// di.Lifecycle (db/cache managers, workers, the HTTP listener registration)
	// resolves and registers its hooks on fx's lifecycle.
	opts = append(opts, fx.Provide(func(fxlc fx.Lifecycle) di.Lifecycle {
		return &lifecycleBridge{fxlc: fxlc}
	}))

	if len(spec.Errs) > 0 {
		opts = append(opts, fx.Error(spec.Errs...))
	}
	if len(spec.Supplies) > 0 {
		opts = append(opts, fx.Supply(spec.Supplies...))
	}
	for _, p := range spec.Provides {
		opts = append(opts, fx.Provide(annotateProvide(p)))
	}
	for _, inv := range spec.Invokes {
		opts = append(opts, fx.Invoke(annotateInvoke(inv)))
	}
	return fx.New(opts...)
}

// annotateProvide reproduces a ProvideSpec's group/param tags using fx.Annotate.
// The raw tag strings di records (`group:"x"`, `optional:"true"`) are exactly
// fx.ResultTags/fx.ParamTags syntax, so they pass through verbatim.
func annotateProvide(p di.ProvideSpec) any {
	anns := make([]fx.Annotation, 0, 2)
	if len(p.ParamTags) > 0 {
		anns = append(anns, fx.ParamTags(p.ParamTags...))
	}
	if len(p.ResultTags) > 0 {
		anns = append(anns, fx.ResultTags(p.ResultTags...))
	}
	if len(anns) == 0 {
		return p.Ctor
	}
	return fx.Annotate(p.Ctor, anns...)
}

// annotateInvoke is the invoke counterpart: only ParamTags apply.
func annotateInvoke(inv di.InvokeSpec) any {
	if len(inv.ParamTags) == 0 {
		return inv.Fn
	}
	return fx.Annotate(inv.Fn, fx.ParamTags(inv.ParamTags...))
}

// lifecycleBridge adapts di.Lifecycle onto fx.Lifecycle, translating each
// di.Hook into an fx.Hook.
type lifecycleBridge struct{ fxlc fx.Lifecycle }

func (b *lifecycleBridge) Append(h di.Hook) {
	b.fxlc.Append(fx.Hook{OnStart: h.OnStart, OnStop: h.OnStop})
}
