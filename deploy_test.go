package nexus

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus/registry"
)

// TestDeployAs_StampsRESTEndpoint registers a REST handler inside a
// Module wrapped with DeployAs and verifies the deployment tag landed on
// the registry.Endpoint entry. This is the primary contract A guarantees
// today — metadata is authoritatively visible without any boot filtering
// or codegen yet.
func TestDeployAs_StampsRESTEndpoint(t *testing.T) {
	fn := func(p Params[struct{}]) (string, error) { return "ok", nil }

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		Module("users",
			DeployAs("users-svc"),
			AsRest("GET", "/users/:id", fn),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	var got *registry.Endpoint
	for i, e := range app.Registry().Endpoints() {
		if e.Path == "/users/:id" {
			got = &app.Registry().Endpoints()[i]
			break
		}
	}
	if got == nil {
		t.Fatal("endpoint not registered")
	}
	if got.Module != "users" {
		t.Errorf("Module: %q, want %q", got.Module, "users")
	}
	if got.Deployment != "users-svc" {
		t.Errorf("Deployment: %q, want %q", got.Deployment, "users-svc")
	}
}

// TestDeployAs_OmittedLeavesEmpty asserts that a Module without DeployAs
// produces endpoints with empty Deployment — so adding the annotation is
// strictly opt-in and existing apps stay unchanged.
func TestDeployAs_OmittedLeavesEmpty(t *testing.T) {
	fn := func(p Params[struct{}]) (string, error) { return "ok", nil }

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		Module("plain",
			AsRest("GET", "/plain", fn),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	for _, e := range app.Registry().Endpoints() {
		if e.Path == "/plain" && e.Deployment != "" {
			t.Fatalf("untagged module shouldn't carry a deployment: %q", e.Deployment)
		}
	}
}

// TestConfig_DeploymentAndVersion_OnDashboardConfig verifies that
// Config.Deployment + Config.Version flow through to /__nexus/config,
// which is what cross-service codegen'd clients will consult to detect
// peer-version skew.
func TestConfig_DeploymentAndVersion_OnDashboardConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{
			Addr:            "127.0.0.1:0",
			EnableDashboard: true,
			Deployment:      "users-svc",
			Version:         "v1.2.3",
		}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/__nexus/config", nil))
	if w.Code != 200 {
		t.Fatalf("status: %d", w.Code)
	}
	var body struct {
		Name       string
		Deployment string
		Version    string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Deployment != "users-svc" {
		t.Errorf("Deployment: %q", body.Deployment)
	}
	if body.Version != "v1.2.3" {
		t.Errorf("Version: %q", body.Version)
	}

	// App accessor mirrors the Config field — useful for client code at
	// runtime that needs to know its own deployment without going through
	// HTTP.
	if app.Deployment() != "users-svc" {
		t.Errorf("App.Deployment(): %q", app.Deployment())
	}
	if app.Version() != "v1.2.3" {
		t.Errorf("App.Version(): %q", app.Version())
	}
}

// TestDeploymentFromEnv reads the env var directly. Captured here so a
// future change that breaks the env-var contract (typo, rename) trips
// the test instead of silently breaking deployments.
func TestDeploymentFromEnv(t *testing.T) {
	t.Setenv("NEXUS_DEPLOYMENT", "billing-svc")
	if got := DeploymentFromEnv(); got != "billing-svc" {
		t.Fatalf("DeploymentFromEnv() = %q, want %q", got, "billing-svc")
	}
	t.Setenv("NEXUS_DEPLOYMENT", "")
	if got := DeploymentFromEnv(); got != "" {
		t.Fatalf("empty env should yield empty string, got %q", got)
	}
}

// TestDeployAs_LastWriteWins documents the behavior when two DeployAs
// markers are both present in one Module. Today this is "last wins" —
// matches how RoutePrefix concatenates left-to-right but for tags the
// last definition is the authoritative one. Locking the contract via
// test so a refactor doesn't accidentally turn it into "first wins".
func TestDeployAs_LastWriteWins(t *testing.T) {
	fn := func(p Params[struct{}]) (string, error) { return "ok", nil }

	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		Module("dual",
			DeployAs("first"),
			DeployAs("second"),
			AsRest("GET", "/dual", fn),
		).nexusOption(),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	for _, e := range app.Registry().Endpoints() {
		if e.Path == "/dual" && e.Deployment != "second" {
			t.Fatalf("expected last DeployAs to win, got %q", e.Deployment)
		}
	}
}

// TestApp_DefaultsVersion_DevWhenUnset captures the default-version
// behavior so a release that wires -ldflags works as expected and an
// unstamped dev build doesn't end up with an empty Version on the wire.
func TestApp_DefaultsVersion_DevWhenUnset(t *testing.T) {
	var app *App
	fxApp := fxtest.New(t,
		fxBootOptions(Config{Addr: "127.0.0.1:0"}),
		fx.Populate(&app),
	)
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	if app.Version() != "dev" {
		t.Fatalf("default Version should be \"dev\", got %q", app.Version())
	}
}
