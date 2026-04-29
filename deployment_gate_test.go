package nexus

import (
	"sync/atomic"
	"testing"

	"go.uber.org/fx"
)

// TestIfDeployment_GateRunsWhenMatch verifies that when the active
// deployment matches one of the listed names, the wrapped opts
// reach the fx graph (the side-effect Invoke fires).
func TestIfDeployment_GateRunsWhenMatch(t *testing.T) {
	t.Setenv("NEXUS_DEPLOYMENT", "web-svc")
	resetDeploymentDefaultsForTest(t)

	var ran int32
	app := fx.New(
		fx.NopLogger,
		IfDeployment([]string{"monolith", "web-svc"},
			Invoke(func() { atomic.StoreInt32(&ran, 1) }),
		).nexusOption(),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("fx.New: %v", err)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatal("expected gated Invoke to run when deployment matches")
	}
}

// TestIfDeployment_GateSkipsWhenMiss verifies that a non-matching
// deployment skips the wrapped opts entirely — the Invoke never
// fires, no constructor runs, no route registers.
func TestIfDeployment_GateSkipsWhenMiss(t *testing.T) {
	t.Setenv("NEXUS_DEPLOYMENT", "uaa-svc")
	resetDeploymentDefaultsForTest(t)

	var ran int32
	app := fx.New(
		fx.NopLogger,
		IfDeployment([]string{"monolith", "web-svc"},
			Invoke(func() { atomic.StoreInt32(&ran, 1) }),
		).nexusOption(),
	)
	if err := app.Err(); err != nil {
		t.Fatalf("fx.New: %v", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatalf("expected gated Invoke to be skipped for non-matching deployment, ran=%d", ran)
	}
}

// TestIfDeployment_EmptyFallsBackToMonolith confirms the
// documented behavior: when no deployment is set anywhere
// (env unset, no codegen baked default), the resolver treats
// the active deployment as "monolith" so plain `go run .`
// against an unannotated build still hits monolith-gated opts.
func TestIfDeployment_EmptyFallsBackToMonolith(t *testing.T) {
	t.Setenv("NEXUS_DEPLOYMENT", "")
	resetDeploymentDefaultsForTest(t)

	if !activeDeploymentMatches([]string{"monolith"}) {
		t.Fatal("expected empty deployment to match 'monolith'")
	}
	if activeDeploymentMatches([]string{"web-svc"}) {
		t.Fatal("empty deployment should not match 'web-svc'")
	}
}

// TestIfDeployment_DefaultsBeatEnvWhenEnvEmpty mirrors the
// runtime priority: NEXUS_DEPLOYMENT env wins, then
// DeploymentDefaults.Deployment, then "monolith" fallback.
// When env is empty but defaults are set (the typical
// `nexus build --deployment uaa-svc` shape), the defaults
// drive matching.
func TestIfDeployment_DefaultsBeatEnvWhenEnvEmpty(t *testing.T) {
	t.Setenv("NEXUS_DEPLOYMENT", "")
	resetDeploymentDefaultsForTest(t)
	SetDeploymentDefaults(DeploymentDefaults{Deployment: "uaa-svc"})

	if activeDeploymentMatches([]string{"monolith"}) {
		t.Fatal("defaults set to uaa-svc but matched 'monolith'")
	}
	if !activeDeploymentMatches([]string{"uaa-svc"}) {
		t.Fatal("defaults set to uaa-svc but did not match 'uaa-svc'")
	}
}

// resetDeploymentDefaultsForTest wipes the package-global
// deployment defaults so consecutive tests don't bleed state
// (SetDeploymentDefaults is process-global). t.Cleanup restores
// the empty state at end-of-test.
func resetDeploymentDefaultsForTest(t *testing.T) {
	t.Helper()
	deployDefaultsMu.Lock()
	deployDefaults = DeploymentDefaults{}
	deployDefaultsOK = false
	deployDefaultsMu.Unlock()
	t.Cleanup(func() {
		deployDefaultsMu.Lock()
		deployDefaults = DeploymentDefaults{}
		deployDefaultsOK = false
		deployDefaultsMu.Unlock()
	})
}
