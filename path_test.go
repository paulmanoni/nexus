package nexus

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"
)

// TestPath_RegistersForServiceLookup verifies the round-trip:
// nexus.Path on the module → modulePublicPath registry →
// (*App).Service constructs a Service whose GraphQLPath is
// rooted at <path>/graphql. This is the contract that lets a
// constructor write app.Service("uaa") and inherit the module's
// public path without needing AtGraphQL.
func TestPath_RegistersForServiceLookup(t *testing.T) {
	resetPublicPathRegistryForTest(t)

	// Build a module that declares Path. The sub-options don't
	// matter for this test — we're checking the side effect of
	// Path on the registry, which Module() applies during the
	// option walk regardless of what else is in the module.
	_ = Module("uaa",
		Path("/oats-uaa"),
	)

	if got := modulePublicPathOf("uaa"); got != "/oats-uaa" {
		t.Fatalf("registry: got %q, want %q", got, "/oats-uaa")
	}

	// app.Service("uaa") should now produce a Service rooted at
	// /oats-uaa/graphql, not the framework default /graphql.
	app := New(Config{})
	svc := app.Service("uaa")
	if svc.GraphQLPath() != "/oats-uaa/graphql" {
		t.Fatalf("Service.GraphQLPath: got %q, want %q",
			svc.GraphQLPath(), "/oats-uaa/graphql")
	}

	// A different service name (no matching module) keeps the
	// framework default — Path is scoped to the module that
	// declared it, not a global REST/GraphQL switch.
	other := app.Service("billing")
	if other.GraphQLPath() != DefaultGraphQLPath {
		t.Fatalf("unrelated service: got %q, want %q",
			other.GraphQLPath(), DefaultGraphQLPath)
	}
}

// TestPath_NormalizesLooseInput ensures Path() accepts the same
// loose forms RoutePrefix already does (no leading slash, trailing
// slash, "/") and stores the canonical "/seg" form so downstream
// consumers (REST mount, GraphQL path) get a consistent shape.
func TestPath_NormalizesLooseInput(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/oats-uaa", "/oats-uaa"},
		{"oats-uaa", "/oats-uaa"},
		{"/oats-uaa/", "/oats-uaa"},
		{"oats-uaa/", "/oats-uaa"},
		{"", ""},
		{"/", ""},
	}
	for _, tc := range cases {
		opt := Path(tc.in)
		got := opt.(pathOption).path
		if got != tc.want {
			t.Errorf("Path(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestPath_AtGraphQLOverrides confirms that an explicit
// (*Service).AtGraphQL(...) AFTER the auto-derivation still wins.
// This matters when one service in a module needs a different
// GraphQL path than the module's public root — Path is the
// default, AtGraphQL is the escape hatch.
func TestPath_AtGraphQLOverrides(t *testing.T) {
	resetPublicPathRegistryForTest(t)
	_ = Module("uaa", Path("/oats-uaa"))

	app := New(Config{})
	svc := app.Service("uaa").AtGraphQL("/custom/graphql")
	if svc.GraphQLPath() != "/custom/graphql" {
		t.Fatalf("AtGraphQL override: got %q, want %q",
			svc.GraphQLPath(), "/custom/graphql")
	}
}

// TestPath_MultiServiceModuleAllMountUnderPath is the regression
// test for the original bug report: when a single module declares
// nexus.Path("/oats-uaa") AND has handlers attached to multiple
// services (e.g. "uaa" + "user" inside the uaa module), every
// field should mount at /oats-uaa/graphql — not just the ones
// whose service name happens to match the module name.
//
// We exercise this through the public surface: build the module
// options, run them through fx end-to-end, and inspect the gin
// engine's registered routes to confirm /oats-uaa/graphql is the
// only GraphQL mount and /graphql is NOT registered.
func TestPath_MultiServiceModuleAllMountUnderPath(t *testing.T) {
	resetPublicPathRegistryForTest(t)

	type uaaArgs struct {
		Token string `graphql:"token"`
	}
	type userArgs struct {
		ID string `graphql:"id"`
	}
	type uaaSvc struct{ *Service }
	type userSvc struct{ *Service }

	newUaaSvc := func(app *App) *uaaSvc { return &uaaSvc{app.Service("uaa")} }
	newUserSvc := func(app *App) *userSvc { return &userSvc{app.Service("user")} }
	newCheckToken := func(_ *uaaSvc, _ Params[uaaArgs]) (string, error) { return "ok", nil }
	newGetUser := func(_ *userSvc, _ Params[userArgs]) (string, error) { return "u", nil }

	mod := Module("uaa",
		Path("/oats-uaa"),
		Provide(newUaaSvc, newUserSvc),
		AsQuery(newCheckToken),
		AsQuery(newGetUser),
	)

	app, err := newApp(Config{}, mod)
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	defer app.Stop()

	routes := app.Engine().Routes()
	var gqlPaths []string
	for _, r := range routes {
		if r.Path == "/oats-uaa/graphql" || r.Path == "/graphql" {
			if !contains(gqlPaths, r.Path) {
				gqlPaths = append(gqlPaths, r.Path)
			}
		}
	}
	if !contains(gqlPaths, "/oats-uaa/graphql") {
		t.Errorf("missing /oats-uaa/graphql mount; routes seen: %v", gqlPaths)
	}
	if contains(gqlPaths, "/graphql") {
		t.Errorf("unexpected /graphql mount — Path should have moved every module field there; routes seen: %v", gqlPaths)
	}
}

// newApp is a tiny test harness mirroring nexus.Run's fx wiring
// (fxBootOptions registers the *App provider and autoMountGraphQL
// Invoke that turns AsQuery/AsMutation declarations into actual
// engine routes) but without binding a listener. Used to drive
// the full mount path in-process and inspect the resulting gin
// route table.
func newApp(cfg Config, opts ...Option) (*testApp, error) {
	var captured *App
	capture := fx.Invoke(func(a *App) { captured = a })
	all := append([]fx.Option{fx.NopLogger, fxBootOptions(cfg), capture}, unwrap(opts)...)
	fxApp := fx.New(all...)
	if err := fxApp.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := fxApp.Start(ctx); err != nil {
		return nil, err
	}
	return &testApp{fx: fxApp, App: captured}, nil
}

type testApp struct {
	*App
	fx *fx.App
}

func (a *testApp) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.fx.Stop(ctx)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// resetPublicPathRegistryForTest wipes the package-global
// modulePublicPath map so consecutive tests don't bleed state.
// Mirrors resetDeploymentDefaultsForTest in deployment_gate_test.go.
func resetPublicPathRegistryForTest(t *testing.T) {
	t.Helper()
	modulePublicPathMu.Lock()
	for k := range modulePublicPath {
		delete(modulePublicPath, k)
	}
	modulePublicPathMu.Unlock()
	t.Cleanup(func() {
		modulePublicPathMu.Lock()
		for k := range modulePublicPath {
			delete(modulePublicPath, k)
		}
		modulePublicPathMu.Unlock()
	})
}
