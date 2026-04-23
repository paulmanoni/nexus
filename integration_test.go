package nexus

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// Users go through nexus.Run; tests poke at the private fxBootOptions so
// we can drive the lifecycle deterministically without blocking on
// signals. Internal fx is still visible here because this file sits in
// the nexus package itself.

func TestRun_StartsAndStops(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Addr:            "127.0.0.1:0",
			EnableDashboard: true,
			TraceCapacity:   100,
			DashboardName:   "Test",
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app == nil {
		t.Fatal("*App not populated")
	}
	if app.Engine() == nil {
		t.Fatal("engine nil")
	}
	if app.Registry() == nil {
		t.Fatal("registry nil")
	}
	if app.Bus() == nil {
		t.Fatal("bus should be enabled when TraceCapacity > 0")
	}
}

func TestRun_BindFailureAbortsStart(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	fxApp := fx.New(
		fx.NopLogger,
		fxBootOptions(Config{Addr: busy.Addr().String()}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fxApp.Start(ctx); err == nil {
		t.Fatal("expected Start to fail when bind is busy")
		_ = fxApp.Stop(ctx)
	}
}

func TestRun_TracingDisabledWhenZero(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0", TraceCapacity: 0}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app.Bus() != nil {
		t.Fatal("bus should be nil when TraceCapacity = 0")
	}
}

func TestOption_UnwrapChain(t *testing.T) {
	// Provide + Invoke + Module compose into a functional fx graph.
	var got string
	mod := Module("t",
		Provide(func() string { return "hello" }),
		Invoke(func(s string) { got = s }),
	)
	fxApp := fxtest.New(t,
		fx.NopLogger,
		mod.nexusOption(),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if got != "hello" {
		t.Errorf("got %q; want hello", got)
	}
}

func TestSupply_IntoGraph(t *testing.T) {
	type tag struct{ name string }
	var seen tag
	mod := Module("supply-test",
		Supply(tag{name: "from-supply"}),
		Invoke(func(t tag) { seen = t }),
	)
	fxApp := fxtest.New(t, fx.NopLogger, mod.nexusOption())
	fxApp.RequireStart()
	defer fxApp.RequireStop()
	if seen.name != "from-supply" {
		t.Errorf("tag = %+v", seen)
	}
}

func TestRaw_AcceptsFxOption(t *testing.T) {
	// Raw gives users access to fx features nexus hasn't mirrored.
	var got int
	mod := Module("raw-test",
		Raw(fx.Provide(func() int { return 42 })),
		Invoke(func(i int) { got = i }),
	)
	fxApp := fxtest.New(t, fx.NopLogger, mod.nexusOption())
	fxApp.RequireStart()
	defer fxApp.RequireStop()
	if got != 42 {
		t.Errorf("got %d", got)
	}
}

// noSvcArgs is a minimal args struct for the zero-service handler test.
type noSvcArgs struct {
	Q string `graphql:"q"`
}

// NewNoSvcQuery is a handler with no *Service dep and no OnService option —
// the case the zero-service fallback must cover.
func NewNoSvcQuery() func(ctx context.Context, a noSvcArgs) (string, error) {
	return func(ctx context.Context, a noSvcArgs) (string, error) {
		return "hello " + a.Q, nil
	}
}

func TestAutoMount_ZeroServiceFallback(t *testing.T) {
	// Handler takes neither *Service nor OnService — with 0 services
	// registered, auto-mount should synthesize a default one rather than
	// failing. Proves the minimal-app case boots.
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		AsQuery(NewNoSvcQuery()).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	endpoints := app.Registry().Endpoints()
	if len(endpoints) == 0 {
		t.Fatal("expected at least one endpoint registered; got none")
	}
	found := false
	for _, e := range endpoints {
		if e.Service == defaultServiceName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected endpoint on default service %q; endpoints=%+v", defaultServiceName, endpoints)
	}
}
