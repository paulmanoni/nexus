package nexus

import (
	"context"
	"testing"

	"go.uber.org/fx"
)

// TestIsDev covers the env-var gate. With NEXUS_DEV unset / not
// "1" the helper reports false; setting it to "1" flips both
// IsDev and the IfDev / IfNotDev branch decision.
func TestIsDev_FalseWhenUnset(t *testing.T) {
	t.Setenv(NexusDevEnv, "")
	if IsDev() {
		t.Error("IsDev should be false when NEXUS_DEV is unset")
	}
}

func TestIsDev_TrueWhenOne(t *testing.T) {
	t.Setenv(NexusDevEnv, "1")
	if !IsDev() {
		t.Error("IsDev should be true when NEXUS_DEV=1")
	}
}

func TestIsDev_FalseForNon1Values(t *testing.T) {
	// Treat any non-"1" value as production. Catches the
	// common "I set NEXUS_DEV=true and it didn't take" gotcha
	// — better to be strict on the sentinel value than to
	// silently flip semantics for typos.
	t.Setenv(NexusDevEnv, "true")
	if IsDev() {
		t.Error("IsDev should treat NEXUS_DEV=true as NOT dev (only \"1\" counts)")
	}
}

// flagInvokeOption returns an Option that flips the passed bool
// when fx executes it. Used to verify IfDev / IfNotDev actually
// skip their wrapped invokes vs. just returning quietly.
func flagInvokeOption(flag *bool) Option {
	return rawOption{o: fx.Invoke(func() { *flag = true })}
}

func TestIfNotDev_AppliesInProduction(t *testing.T) {
	t.Setenv(NexusDevEnv, "")
	var fired bool
	opt := IfNotDev(flagInvokeOption(&fired))

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !fired {
		t.Error("IfNotDev should have applied its options in production")
	}
}

func TestIfNotDev_SkipsInDev(t *testing.T) {
	t.Setenv(NexusDevEnv, "1")
	var fired bool
	opt := IfNotDev(flagInvokeOption(&fired))

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if fired {
		t.Error("IfNotDev should NOT have applied its options under NEXUS_DEV=1")
	}
}

func TestIfDev_AppliesInDev(t *testing.T) {
	t.Setenv(NexusDevEnv, "1")
	var fired bool
	opt := IfDev(flagInvokeOption(&fired))

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !fired {
		t.Error("IfDev should have applied its options under NEXUS_DEV=1")
	}
}

func TestIfDev_SkipsInProduction(t *testing.T) {
	t.Setenv(NexusDevEnv, "")
	var fired bool
	opt := IfDev(flagInvokeOption(&fired))

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if fired {
		t.Error("IfDev should NOT have applied its options in production")
	}
}

// TestIfNotDev_VariadicComposesMultipleOptions: a single
// IfNotDev gates a whole batch. Confirms the variadic shape
// works as documented (one wrapper, many real options) so
// operators don't have to call Options(...) explicitly.
func TestIfNotDev_VariadicComposesMultipleOptions(t *testing.T) {
	t.Setenv(NexusDevEnv, "")
	var a, b, c bool
	opt := IfNotDev(
		flagInvokeOption(&a),
		flagInvokeOption(&b),
		flagInvokeOption(&c),
	)

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
	if !a || !b || !c {
		t.Errorf("all three invokes should have fired, got a=%v b=%v c=%v", a, b, c)
	}
}

// TestIfNotDev_EmptyInputIsNoop covers the edge case where the
// caller passes no options. Must NOT crash; the resulting
// option is a no-op.
func TestIfNotDev_EmptyInputIsNoop(t *testing.T) {
	t.Setenv(NexusDevEnv, "")
	opt := IfNotDev()

	app := fx.New(fx.NopLogger, unwrap([]Option{opt})[0])
	defer app.Stop(context.Background())
	if err := app.Start(context.Background()); err != nil {
		t.Fatalf("fx start: %v", err)
	}
}
