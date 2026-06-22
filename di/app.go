package di

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// Hook is a pair of lifecycle callbacks. OnStart runs when the app starts;
// OnStop runs (in reverse append order) when it stops. Either may be nil.
// Mirrors fx.Hook.
type Hook struct {
	OnStart func(context.Context) error
	OnStop  func(context.Context) error
}

// Lifecycle collects start/stop hooks. A constructor or invoke takes it as a
// parameter to register resource startup/shutdown — it is always resolvable
// without an explicit Provide. Mirrors fx.Lifecycle.
type Lifecycle interface{ Append(Hook) }

type lifecycle struct{ hooks []Hook }

func (l *lifecycle) Append(h Hook) { l.hooks = append(l.hooks, h) }

// App is a built container ready to Start/Stop or Run. New runs all Invokes
// eagerly; any error from building options, a constructor, or an invoke is
// captured and returned by Err (and aborts Run).
type App struct {
	c   *container
	err error
}

// New builds the container from opts. Provides/supplies register first
// (order-independent), then invokes run in tree order. The first error
// encountered short-circuits the rest and is surfaced via Err.
func New(opts ...Option) *App {
	return Build(Collect(opts...))
}

// Build constructs an App from an already-collected Spec. Backends and tests
// that hold a Spec use this directly; New is sugar over Collect+Build.
func Build(spec *Spec) *App {
	c := newContainer()
	app := &App{c: c}

	// Option-time errors (di.Error) abort before any construction, matching fx.
	if len(spec.Errs) > 0 {
		app.err = errors.Join(spec.Errs...)
		return app
	}
	for _, v := range spec.Supplies {
		if err := c.supply(v); err != nil {
			app.err = err
			return app
		}
	}
	for _, p := range spec.Provides {
		if err := c.register(p); err != nil {
			app.err = err
			return app
		}
	}
	for _, inv := range spec.Invokes {
		if err := c.invoke(inv); err != nil {
			app.err = err
			return app
		}
	}
	return app
}

// Err reports the first error from building/invoking, or nil.
func (a *App) Err() error { return a.err }

// Start runs every registered OnStart hook in append order. The first failure
// rolls back already-started hooks (their OnStop runs, reverse order) and
// returns the error — matching fx's start-failure rollback.
func (a *App) Start(ctx context.Context) error {
	if a.err != nil {
		return a.err
	}
	hooks := a.c.lc.hooks
	for i, h := range hooks {
		if h.OnStart == nil {
			continue
		}
		if err := h.OnStart(ctx); err != nil {
			a.rollback(ctx, i)
			return fmt.Errorf("di: OnStart hook %d failed: %w", i, err)
		}
	}
	return nil
}

// rollback runs OnStop for hooks [0,upto) in reverse, used when a later OnStart
// fails mid-start.
func (a *App) rollback(ctx context.Context, upto int) {
	hooks := a.c.lc.hooks
	for i := upto - 1; i >= 0; i-- {
		if hooks[i].OnStop != nil {
			_ = hooks[i].OnStop(ctx)
		}
	}
}

// Stop runs every OnStop hook in reverse append order, joining any errors.
func (a *App) Stop(ctx context.Context) error {
	hooks := a.c.lc.hooks
	var errs []error
	for i := len(hooks) - 1; i >= 0; i-- {
		if hooks[i].OnStop != nil {
			if err := hooks[i].OnStop(ctx); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Run starts the app, blocks until SIGINT/SIGTERM, then stops it gracefully.
// Build/invoke/start errors are written to stderr and exit non-zero, matching
// fx.App.Run's fail-fast behavior.
func (a *App) Run() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if a.err != nil {
		fmt.Fprintf(os.Stderr, "nexus/di: %v\n", a.err)
		os.Exit(1)
	}
	if err := a.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "nexus/di: %v\n", err)
		os.Exit(1)
	}
	<-ctx.Done()
	if err := a.Stop(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "nexus/di: shutdown: %v\n", err)
		os.Exit(1)
	}
}
