package template

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/paulmanoni/nexus/live"
)

// startedApp builds and starts an fx.App with the given options,
// failing the test on any error. The returned stop func tears the
// app down within a bounded timeout — tests must call it via defer
// or risk leaking goroutines into other tests.
func startedApp(t *testing.T, opts ...fx.Option) (*fx.App, func()) {
	t.Helper()
	app := fx.New(append(opts, fx.NopLogger)...)
	startCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return app, func() {
		stopCtx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = app.Stop(stopCtx)
	}
}

func TestFx_Module_ProvidesEngine(t *testing.T) {
	var got *Engine
	_, stop := startedApp(t,
		fx.Provide(live.New),
		fx.Provide(NewEngine),
		fx.Populate(&got),
	)
	defer stop()

	if got == nil {
		t.Fatal("Engine was not provided into the graph")
	}
	if got.notifier == nil {
		t.Error("Engine should carry the *live.Notifier from the graph")
	}
}

func TestFx_RegisterComponent_RegistersAtStartup(t *testing.T) {
	var engine *Engine
	_, stop := startedApp(t,
		fx.Provide(live.New),
		fx.Provide(NewEngine),
		RegisterComponent("Counter", []byte(counterTmpl), func() Component { return &counterComponent{} }),
		fx.Populate(&engine),
	)
	defer stop()

	if _, ok := engine.lookup("Counter"); !ok {
		t.Fatal("Counter component should be registered after fx.Start")
	}
}

func TestFx_RegisterComponent_PropagatesParseError(t *testing.T) {
	// Malformed template — fx.Start should fail with the parse error
	// bubbled up from RegisterComponent's Invoke.
	app := fx.New(
		fx.NopLogger,
		fx.Provide(live.New),
		fx.Provide(NewEngine),
		RegisterComponent("Bad", []byte(`<template`), func() Component { return &counterComponent{} }),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := app.Start(ctx)
	if err == nil {
		_ = app.Stop(context.Background())
		t.Fatal("expected fx.Start to fail on malformed template")
	}
}

// --- TemplateNamer / adapter path resolution -----------------------

// namerComp implements TemplateNamer so the adapter resolves its
// template path via the method rather than spec.TemplatePath or the
// convention fallback. The Ctx is nil at registration time — the
// implementation must not deref it.
type namerComp struct{ BaseComponent }

func (c *namerComp) TemplateName(_ *Ctx) string { return "custom/MyNamer" }

func TestAdapter_PathResolution_TemplateNamer(t *testing.T) {
	got := resolveTemplatePath("MyNamer", "", func() any { return &namerComp{} })
	if got != "custom/MyNamer.nlt" {
		t.Errorf("namer path = %q, want %q", got, "custom/MyNamer.nlt")
	}
}

func TestAdapter_PathResolution_ConventionFallback(t *testing.T) {
	// No TemplateNamer, no WithTemplate → "templates/<Name>.nlt".
	got := resolveTemplatePath("Hello", "", func() any { return &counterComponent{} })
	if got != "templates/Hello.nlt" {
		t.Errorf("convention path = %q, want %q", got, "templates/Hello.nlt")
	}
}

func TestAdapter_PathResolution_ExplicitWithTemplateWins(t *testing.T) {
	got := resolveTemplatePath("MyNamer", "explicit/path", func() any { return &namerComp{} })
	if got != "explicit/path.nlt" {
		t.Errorf("explicit path = %q, want %q", got, "explicit/path.nlt")
	}
}

func TestAdapter_PathResolution_AcceptsExistingNltSuffix(t *testing.T) {
	got := resolveTemplatePath("X", "already/has.nlt", func() any { return &counterComponent{} })
	if got != "already/has.nlt" {
		t.Errorf("path with .nlt = %q, want %q", got, "already/has.nlt")
	}
}

// Verifies the dep-injected-factory pattern documented in
// RegisterComponent's godoc actually works end-to-end: a constructor
// closes over a repo and the engine ends up registered with a
// factory that produces components carrying that repo.
type fakeRepo struct{ Name string }

type repoComponent struct {
	BaseComponent
	Repo *fakeRepo
}

func (c *repoComponent) Mount(_ *Ctx) error { return nil }

func TestFx_DepInjectedFactoryPattern(t *testing.T) {
	var engine *Engine
	_, stop := startedApp(t,
		fx.Provide(live.New),
		fx.Provide(func() *fakeRepo { return &fakeRepo{Name: "loaded"} }),
		fx.Provide(NewEngine),
		fx.Invoke(func(e *Engine, repo *fakeRepo) error {
			return e.Register("Repo", []byte(`<template>{{ Repo.Name }}</template>`),
				func() Component { return &repoComponent{Repo: repo} },
			)
		}),
		fx.Populate(&engine),
	)
	defer stop()

	def, ok := engine.lookup("Repo")
	if !ok {
		t.Fatal("Repo not registered")
	}
	// Instantiate a session-style fresh component and render — the
	// repo should be reachable via the closure.
	comp := def.factory().(*repoComponent)
	if comp.Repo == nil || comp.Repo.Name != "loaded" {
		t.Errorf("factory didn't capture repo; got %+v", comp.Repo)
	}
	if got := Render(def.fragment, comp, WithComponents(engine)).HTML(); got != "loaded" {
		t.Errorf("render = %q want %q", got, "loaded")
	}
}
