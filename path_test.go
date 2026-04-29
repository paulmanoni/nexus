package nexus

import (
	"testing"
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
