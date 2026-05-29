package nexus

import (
	"context"
	"fmt"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/paulmanoni/nexus/resource"
)

// Managed is the general-purpose declarative binder for any resource
// manager that doesn't fit the kind-specific Database[T] / Cache[T] /
// pubsub.Broker[T] (e.g. a custom RabbitMQ/queue manager, an object
// store client, a third-party SDK wrapper).
//
// Unlike Database[T], the caller constructs the whole *T in build — so
// T can have arbitrary extra fields, not just an embedded manager — and
// returns an error to fail boot when the resource is required. The
// framework then:
//
//   - manages lifecycle automatically: on boot it calls Start() if *T
//     has one; on shutdown it calls Stop() (or Close()). Both void and
//     error-returning forms are detected, so db/cache/queue managers
//     (Start()/Stop()) and transport-style handles (Close() error) all
//     work without the caller wiring hooks.
//   - registers a dashboard resource via resourceFor (pass nil to skip
//     — useful for a managed handle that isn't a topology node).
//
//	type RabbitMQ struct {
//	    *rabbitmq.RabbitMQManager
//	    cfg *rabbitmq.RabbitMQConfig
//	}
//
//	nexus.Managed("rabbitmq",
//	    func(logger *zap.Logger) (*RabbitMQ, error) {
//	        cfg := rabbitmq.NewRabbitMQConfig()
//	        return &RabbitMQ{rabbitmq.NewRabbitMQManager(cfg, logger), cfg}, nil
//	    },
//	    func(r *RabbitMQ) resource.Resource {
//	        return resource.NewQueue("rabbitmq", "RabbitMQ broker",
//	            map[string]any{"broker": "rabbitmq"}, r.IsConnected)
//	    },
//	)
//
// Handlers inject *RabbitMQ unchanged.
func Managed[T any](name string, build func(*zap.Logger) (*T, error), resourceFor func(*T) resource.Resource) Option {
	if name == "" {
		panic("nexus.Managed: name must not be empty")
	}
	if build == nil {
		panic("nexus.Managed: build func must not be nil")
	}

	ctor := func(lc fx.Lifecycle, logger *zap.Logger) (*T, error) {
		t, err := build(logger)
		if err != nil {
			return nil, fmt.Errorf("nexus.Managed[%q]: %w", name, err)
		}
		if t == nil {
			return nil, fmt.Errorf("nexus.Managed[%q]: build returned a nil handle", name)
		}
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { return runManagedStart(t) },
			OnStop:  func(context.Context) error { return runManagedStop(t) },
		})
		return t, nil
	}

	register := func(app *App, t *T) {
		if resourceFor != nil {
			app.Register(resourceFor(t))
		}
	}

	return rawOption{o: fx.Options(fx.Provide(ctor), fx.Invoke(register))}
}

// runManagedStart invokes the handle's start lifecycle if it has one.
// Both Start() and Start() error are recognized (promoted methods from
// an embedded manager count); a handle with neither is a no-op.
func runManagedStart(t any) error {
	switch s := t.(type) {
	case interface{ Start() error }:
		return s.Start()
	case interface{ Start() }:
		s.Start()
	}
	return nil
}

// runManagedStop invokes the handle's stop/close lifecycle if it has
// one, preferring Stop over Close. Both void and error forms work.
func runManagedStop(t any) error {
	switch s := t.(type) {
	case interface{ Stop() error }:
		return s.Stop()
	case interface{ Stop() }:
		s.Stop()
	case interface{ Close() error }:
		return s.Close()
	case interface{ Close() }:
		s.Close()
	}
	return nil
}
