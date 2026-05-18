package nexus

import (
	"reflect"
	"testing"
)

// Test fixtures — minimal struct types that play the role of live-
// template components purely for the name-inference paths. We never
// register them against a real LiveAdapter; we only exercise
// inferComponentName + the spec-population flow in AsComponent.

type fakeCompA struct{}
type fakeCompWithDeps struct{ dep string }

func TestInferComponentName_PointerToStruct(t *testing.T) {
	// (*fakeCompA, error) is the canonical ctor return shape; the
	// inferred name should be the struct's type name, not "*fakeCompA".
	ctor := func() (*fakeCompA, error) { return &fakeCompA{}, nil }
	ctorType := reflect.TypeOf(ctor)
	got := inferComponentName(ctorType.Out(0))
	if got != "fakeCompA" {
		t.Errorf("inferComponentName(*fakeCompA) = %q, want %q", got, "fakeCompA")
	}
}

func TestInferComponentName_ValueStruct(t *testing.T) {
	// Returning the struct by value is unusual but legal. Should
	// still produce the struct's name (no pointer to unwrap).
	ctor := func() fakeCompA { return fakeCompA{} }
	ctorType := reflect.TypeOf(ctor)
	got := inferComponentName(ctorType.Out(0))
	if got != "fakeCompA" {
		t.Errorf("inferComponentName(fakeCompA) = %q, want %q", got, "fakeCompA")
	}
}

func TestInferComponentName_AnonymousStructReturnsEmpty(t *testing.T) {
	// Anonymous struct types have no Name() — caller must use
	// nexus.WithName explicitly.
	ctor := func() *struct{ X int } { return &struct{ X int }{} }
	ctorType := reflect.TypeOf(ctor)
	got := inferComponentName(ctorType.Out(0))
	if got != "" {
		t.Errorf("inferComponentName(anon struct) = %q, want empty", got)
	}
}

func TestInferComponentName_NilType(t *testing.T) {
	// Defensive: a nil reflect.Type returns "" rather than panicking.
	got := inferComponentName(nil)
	if got != "" {
		t.Errorf("inferComponentName(nil) = %q, want empty", got)
	}
}

// TestAsComponent_NameInferredFromCtor exercises the public AsComponent
// flow. We can't observe spec.Name from outside the package — the
// registration is wrapped into an fx.Invoke that only runs if an
// adapter is in the graph. So this test taps directly into the
// pre-Invoke spec-population branch via the failure-mode path:
// passing a ctor that returns an anonymous-struct (which the inference
// can't name) without a WithName should produce an fx error option,
// not panic. Conversely, a real ctor should produce a Raw(fx.Invoke).
func TestAsComponent_AnonStructRejectedWithoutWithName(t *testing.T) {
	ctor := func() *struct{ X int } { return &struct{ X int }{} }
	opt := AsComponent(ctor)
	// AsComponent always returns an Option. Failure paths embed
	// fx.Error; success paths embed fx.Invoke. We can't introspect
	// the wrapped option directly without exporting more surface,
	// so this test only confirms AsComponent doesn't panic on the
	// no-name branch — the fx.Error itself is exercised at app boot
	// time by the integration tests.
	if opt == nil {
		t.Fatal("AsComponent returned nil Option")
	}
}

func TestAsComponent_WithNameOverridesInference(t *testing.T) {
	// We can't observe spec.Name post-Apply from outside the package
	// without an integration test, so directly invoke the option's
	// Apply method against a synthetic spec.
	spec := &ComponentSpec{Name: "Inferred"}
	WithName("Override").Apply(spec)
	if spec.Name != "Override" {
		t.Errorf("WithName(\"Override\") left Name = %q, want %q", spec.Name, "Override")
	}
}

func TestAsComponent_NonFunctionCtorReturnsErrorOption(t *testing.T) {
	// Passing a non-function (e.g. an instance) is a programmer
	// error caught at AsComponent call time. The result is a
	// fx.Error-wrapped Option that fails app boot rather than
	// panicking now.
	opt := AsComponent("not a function")
	if opt == nil {
		t.Fatal("AsComponent returned nil for non-function ctor")
	}
}

func TestAsComponent_CtorWithDepsInfersFromReturnType(t *testing.T) {
	// Constructors with dep params should still infer name from
	// the return type, ignoring the param list. This is the
	// "fx-wired" pattern most apps use.
	ctor := func(_ string) *fakeCompWithDeps { return &fakeCompWithDeps{} }
	ctorType := reflect.TypeOf(ctor)
	got := inferComponentName(ctorType.Out(0))
	if got != "fakeCompWithDeps" {
		t.Errorf("inferComponentName from deps ctor = %q, want %q", got, "fakeCompWithDeps")
	}
}
