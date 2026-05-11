package nexus

import (
	"errors"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/paulmanoni/nexus/manifest"
)

// declareInputs is the canonical setup for boot-wiring tests: declare
// an environment, a required env var, a validated env var, and a
// secret so each validation path has something to exercise.
func declareInputs(t *testing.T, env string, extras ...func(app *App)) *App {
	t.Helper()
	min32 := 32
	cfg := Config{
		Environment: env,
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	var app *App
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareEnvironment(manifest.Environment{Name: "production"})
		a.DeclareEnvironment(manifest.Environment{Name: "staging"})
		a.DeclareEnv(manifest.EnvVar{
			Name:    "LOG_LEVEL",
			Default: "info",
			Validation: &manifest.EnvValidation{
				Enum: []string{"debug", "info", "warn", "error"},
			},
		})
		a.DeclareEnv(manifest.EnvVar{
			Name:     "API_BASE_URL",
			Required: true,
			Default:  "http://localhost:8080",
		})
		a.DeclareSecret(manifest.Secret{
			Name:     "JWT_SIGNING_KEY",
			Required: true,
			Validation: &manifest.EnvValidation{
				Length: &manifest.Range{Min: &min32},
			},
		})
		for _, fn := range extras {
			fn(a)
		}
	})
	fxApp := fxtest.New(t,
		fxBootOptions(cfg),
		declareOpt,
		fx.Populate(&app),
	)
	t.Cleanup(func() {
		if app != nil && fxApp != nil {
			_ = fxApp.Stop(t.Context())
		}
	})
	if err := fxApp.Start(t.Context()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	return app
}

func TestEnvironment_DefaultsToProduction(t *testing.T) {
	cfg := Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}
	got := resolveConfig(cfg).Environment
	if got != "production" {
		t.Errorf("Environment default: got %q, want production", got)
	}
}

func TestEnvironment_FromConfigField_Wins(t *testing.T) {
	cfg := Config{
		Environment: "staging",
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	t.Setenv(nexusEnvironmentEnv, "production")
	got := resolveConfig(cfg).Environment
	if got != "staging" {
		t.Errorf("explicit Config.Environment should win: got %q", got)
	}
}

func TestEnvironment_FromEnvVar(t *testing.T) {
	t.Setenv(nexusEnvironmentEnv, "preview")
	got := resolveConfig(Config{Server: ServerConfig{Addr: "127.0.0.1:0"}}).Environment
	if got != "preview" {
		t.Errorf("NEXUS_ENVIRONMENT should populate Environment: got %q", got)
	}
}

func TestResolveEffective_BootSucceeds_WhenAllInputsPresent(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", strings.Repeat("a", 32))
	app := declareInputs(t, "production")
	if app.Environment() != "production" {
		t.Errorf("Environment: got %q, want production", app.Environment())
	}
	if app.EffectiveManifest() == nil {
		t.Fatal("EffectiveManifest should be populated after boot")
	}
}

func TestResolveEffective_BootFails_WhenRequiredSecretMissing(t *testing.T) {
	// Deliberately leave JWT_SIGNING_KEY unset to force the failure.
	t.Setenv("JWT_SIGNING_KEY", "")
	cfg := Config{
		Environment: "production",
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareEnvironment(manifest.Environment{Name: "production"})
		a.DeclareSecret(manifest.Secret{Name: "JWT_SIGNING_KEY", Required: true})
	})
	fxApp := fx.New(fxBootOptions(cfg), declareOpt)
	err := fxApp.Start(t.Context())
	if err == nil {
		_ = fxApp.Stop(t.Context())
		t.Fatal("expected boot to fail when required secret is missing")
	}
	var ue *UserError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *UserError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.Msg, "JWT_SIGNING_KEY") {
		t.Errorf("error should name the missing secret: %v", ue)
	}
}

func TestResolveEffective_BootFails_WhenSecretViolatesLength(t *testing.T) {
	// Provide a too-short key.
	t.Setenv("JWT_SIGNING_KEY", "short")
	cfg := Config{
		Environment: "production",
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	min32 := 32
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareEnvironment(manifest.Environment{Name: "production"})
		a.DeclareSecret(manifest.Secret{
			Name:     "JWT_SIGNING_KEY",
			Required: true,
			Validation: &manifest.EnvValidation{
				Length: &manifest.Range{Min: &min32},
			},
		})
	})
	fxApp := fx.New(fxBootOptions(cfg), declareOpt)
	err := fxApp.Start(t.Context())
	if err == nil {
		_ = fxApp.Stop(t.Context())
		t.Fatal("expected boot to fail on length violation")
	}
	if !strings.Contains(err.Error(), "length.min") {
		t.Errorf("error should cite the violated rule: %v", err)
	}
}

func TestResolveEffective_BootFails_WhenEnvViolatesEnum(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", strings.Repeat("a", 32))
	t.Setenv("LOG_LEVEL", "trace") // not in enum [debug info warn error]
	cfg := Config{
		Environment: "production",
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareEnvironment(manifest.Environment{Name: "production"})
		a.DeclareEnv(manifest.EnvVar{
			Name: "LOG_LEVEL",
			Validation: &manifest.EnvValidation{
				Enum: []string{"debug", "info", "warn", "error"},
			},
		})
	})
	fxApp := fx.New(fxBootOptions(cfg), declareOpt)
	err := fxApp.Start(t.Context())
	if err == nil {
		_ = fxApp.Stop(t.Context())
		t.Fatal("expected boot to fail on enum violation")
	}
	if !strings.Contains(err.Error(), "enum") {
		t.Errorf("error should cite enum violation: %v", err)
	}
}

func TestResolveEffective_OverrideAppliesPerEnv(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", strings.Repeat("a", 32))
	app := declareInputs(t, "staging", func(a *App) {
		a.DeclareOverride("staging", manifest.Override{
			Env: map[string]string{"LOG_LEVEL": "debug"},
		})
	})
	eff := app.EffectiveManifest()
	if eff == nil {
		t.Fatal("EffectiveManifest nil")
	}
	for _, e := range eff.Env {
		if e.Name == "LOG_LEVEL" {
			if e.Default != "debug" {
				t.Errorf("LOG_LEVEL: got %q, want debug (override should win)", e.Default)
			}
			if e.Source != "override" {
				t.Errorf("LOG_LEVEL.Source: got %q, want override", e.Source)
			}
			return
		}
	}
	t.Fatal("LOG_LEVEL not found in effective manifest")
}

func TestResolveEffective_UndeclaredEnvironmentWithOverrides_Rejected(t *testing.T) {
	t.Setenv("JWT_SIGNING_KEY", strings.Repeat("a", 32))
	cfg := Config{
		Environment: "qa", // not declared
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareEnvironment(manifest.Environment{Name: "production"})
		// Declare an override so the strict-declared-env check fires.
		a.DeclareOverride("production", manifest.Override{})
	})
	fxApp := fx.New(fxBootOptions(cfg), declareOpt)
	err := fxApp.Start(t.Context())
	if err == nil {
		_ = fxApp.Stop(t.Context())
		t.Fatal("expected boot to fail on undeclared environment with overrides present")
	}
	if !strings.Contains(err.Error(), "qa") {
		t.Errorf("error should name the undeclared env: %v", err)
	}
}

func TestResolveEffective_UndeclaredEnvironmentNoOverrides_Tolerated(t *testing.T) {
	// When no overrides are declared at all, an "unknown" env should
	// be treated as a no-op (the app is using nexus pre-cloud and
	// doesn't care about per-env semantics).
	t.Setenv("JWT_SIGNING_KEY", strings.Repeat("a", 32))
	cfg := Config{
		Environment: "qa",
		Server:      ServerConfig{Addr: "127.0.0.1:0"},
	}
	var app *App
	declareOpt := fx.Invoke(func(a *App) {
		a.DeclareSecret(manifest.Secret{Name: "JWT_SIGNING_KEY", Required: true})
	})
	fxApp := fxtest.New(t, fxBootOptions(cfg), declareOpt, fx.Populate(&app))
	if err := fxApp.Start(t.Context()); err != nil {
		t.Fatalf("expected boot to succeed without declared overrides: %v", err)
	}
	defer func() { _ = fxApp.Stop(t.Context()) }()
	if app.EffectiveManifest() == nil {
		t.Fatal("EffectiveManifest should be set even when env was synthesized")
	}
}
