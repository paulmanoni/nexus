package nexus

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus/registry"
	"github.com/paulmanoni/nexus/resource"
)

// Users go through nexus.Run; tests poke at the private fxBootOptions so
// we can drive the lifecycle deterministically without blocking on
// signals. Internal fx is still visible here because this file sits in
// the nexus package itself.

func TestRun_StartsAndStops(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Server:        ServerConfig{Addr: "127.0.0.1:0"},
			Dashboard:     DashboardConfig{Enabled: true, Name: "Test"},
			TraceCapacity: 100,
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
		fxBootOptions(Config{Server: ServerConfig{Addr: busy.Addr().String()}}),
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
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}, TraceCapacity: 0}),
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

func TestAutoMount_StampsModuleName(t *testing.T) {
	// nexus.Module("adverts", AsQuery(...)) must propagate "adverts" to
	// the endpoint's Module field in the registry.
	var app *App
	mod := Module("adverts",
		AsQuery(NewNoSvcQuery()),
	)
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		mod.nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	endpoints := app.Registry().Endpoints()
	if len(endpoints) == 0 {
		t.Fatal("expected an endpoint registered")
	}
	for _, e := range endpoints {
		if e.Module != "adverts" {
			t.Errorf("endpoint %q: Module = %q; want %q", e.Name, e.Module, "adverts")
		}
	}
}

// fakeDB is a minimal NexusResourceProvider for the ProvideService test.
type fakeDB struct{}

func (f *fakeDB) NexusResources() []resource.Resource {
	return []resource.Resource{resource.NewDatabase("main", "test", nil, func() bool { return true })}
}

// UsersService is a service wrapper used to prove ProvideService
// detects service-to-service deps at the constructor level.
type UsersService struct{ *Service }

// AdvertsService takes another service + a resource provider — exactly
// the pattern the user flagged ("NewAdvertsService(app, users, db)").
type AdvertsService struct{ *Service }

func NewUsersService(app *App) *UsersService {
	return &UsersService{Service: app.Service("users")}
}
func NewAdvertsService(app *App, users *UsersService, db *fakeDB) *AdvertsService {
	return &AdvertsService{Service: app.Service("adverts")}
}

// testRestHandlerCtrl is the minimal factory-consumable dep for the
// AsRestHandler smoke test. Real controllers (DeviceController,
// DeploymentController) are the use-case this shape targets.
type testRestHandlerCtrl struct{ counter *int }

func (c *testRestHandlerCtrl) Ping(gc *gin.Context) {
	*c.counter++
	gc.JSON(200, gin.H{"ok": true})
}

func TestAsRestHandler_MountsFactoryHandler(t *testing.T) {
	var counter int
	ctrl := &testRestHandlerCtrl{counter: &counter}

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Supply(ctrl).nexusOption(),
		AsRestHandler("GET", "/ping",
			func(c *testRestHandlerCtrl) gin.HandlerFunc { return c.Ping },
			Description("ping"),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Verify the endpoint landed in the registry with REST transport
	// and a metrics middleware chip (the dashboard-animation enabler).
	found := false
	for _, e := range app.Registry().Endpoints() {
		if e.Transport != "rest" || e.Path != "/ping" {
			continue
		}
		found = true
		var sawMetrics bool
		for _, m := range e.Middleware {
			if m == "metrics" {
				sawMetrics = true
			}
		}
		if !sawMetrics {
			t.Errorf("rest endpoint missing metrics middleware; chain=%v", e.Middleware)
		}
	}
	if !found {
		t.Fatal("AsRestHandler did not register the endpoint")
	}
}

func TestProvideService_RecordsConstructorDeps(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
		Provide(func() *fakeDB { return &fakeDB{} }).nexusOption(),
		Provide(NewUsersService).nexusOption(),
		Provide(NewAdvertsService).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	var adverts *registry.Service
	for _, s := range app.Registry().Services() {
		if s.Name == "adverts" {
			adverts = &s
		}
	}
	if adverts == nil {
		t.Fatal("adverts service not registered")
	}
	// ResourceDeps comes from fakeDB.NexusResources() — "main".
	if len(adverts.ResourceDeps) != 1 || adverts.ResourceDeps[0] != "main" {
		t.Errorf("expected ResourceDeps=[main]; got %v", adverts.ResourceDeps)
	}
	// ServiceDeps comes from *UsersService param.
	if len(adverts.ServiceDeps) != 1 || adverts.ServiceDeps[0] != "users" {
		t.Errorf("expected ServiceDeps=[users]; got %v", adverts.ServiceDeps)
	}
}

func TestAutoMount_ZeroServiceFallback(t *testing.T) {
	// Handler takes neither *Service nor OnService — with 0 services
	// registered, auto-mount should synthesize a default one rather than
	// failing. Proves the minimal-app case boots.
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}),
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
