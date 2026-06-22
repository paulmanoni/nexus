package di

import "context"

// Instance is a built app a Backend hands back: it can report build errors and
// drive the lifecycle. *App satisfies it; the fx adapter returns its own type.
type Instance interface {
	Err() error
	Start(context.Context) error
	Stop(context.Context) error
	Run()
}

// Backend turns a collected Spec into a runnable Instance. The default is
// Builtin (this package's container); nexus/di/fxcontainer is an alternative
// that translates the same Spec onto go.uber.org/fx. Selected in nexus via
// WithContainer — exactly like the httpx router seam.
type Backend interface {
	Build(*Spec) Instance
}

type builtinBackend struct{}

// Builtin is the default Backend: the zero-dependency container in this
// package.
func Builtin() Backend { return builtinBackend{} }

func (builtinBackend) Build(s *Spec) Instance { return Build(s) }
