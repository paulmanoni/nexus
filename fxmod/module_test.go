package fxmod

import (
	"context"
	"net"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus"
)

func TestModule_StartsAndStops(t *testing.T) {
	var app *nexus.App
	fxApp := fxtest.New(t,
		fx.Supply(Config{
			Addr:            "127.0.0.1:0", // random port
			EnableDashboard: true,
			TraceCapacity:   100,
			DashboardName:   "Test",
		}),
		Module,
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app == nil {
		t.Fatal("*nexus.App not populated")
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

func TestModule_BindFailureAbortsStart(t *testing.T) {
	// Grab a port, hold it busy, ask the module to bind there.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()

	fxApp := fx.New(
		fx.NopLogger,
		fx.Supply(Config{Addr: busy.Addr().String()}),
		Module,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := fxApp.Start(ctx); err == nil {
		t.Fatal("expected Start to fail when bind is busy")
		_ = fxApp.Stop(ctx)
	}
}

func TestModule_TracingDisabledWhenZero(t *testing.T) {
	var app *nexus.App
	fxApp := fxtest.New(t,
		fx.Supply(Config{Addr: "127.0.0.1:0", TraceCapacity: 0}),
		Module,
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app.Bus() != nil {
		t.Fatal("bus should be nil when TraceCapacity = 0")
	}
}
