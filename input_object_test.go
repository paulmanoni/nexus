package nexus

import (
	"reflect"
	"testing"
)

// Args struct with the same shape as graphapp's CreateAdvertArgs.
type inputTestArgs struct {
	Title        string `graphql:"title,required" validate:"required,len=3|120"`
	EmployerName string `graphql:"employerName,required" validate:"required,len=2|200"`
}

// Wrapper parallel to the graphapp handler's signature — anonymous struct
// with a single exported field whose type is the real args struct.
type inputTestWrapper = struct {
	Input inputTestArgs
}

func TestDetectInputObject_AnonWrapperInsideParams(t *testing.T) {
	// Simulate what inspectHandler extracts as argsType for a handler
	// declared `p nexus.Params[struct{ Input inputTestArgs }]`.
	argsType := reflect.TypeOf(inputTestWrapper{})

	name, inner, ok := detectInputObject(argsType)
	if !ok {
		t.Fatalf("detectInputObject returned ok=false for %v (NumField=%d kind=%s)",
			argsType, argsType.NumField(), argsType.Kind())
	}
	if name != "input" {
		t.Errorf("arg name = %q; want input", name)
	}
	if inner != reflect.TypeOf(inputTestArgs{}) {
		t.Errorf("inner = %v; want inputTestArgs", inner)
	}
}

func TestDetectInputObject_DoesNotMatchFlat(t *testing.T) {
	// Plain args struct (flat) should NOT trigger input-object mode —
	// fields are primitives, not structs.
	argsType := reflect.TypeOf(inputTestArgs{})
	if _, _, ok := detectInputObject(argsType); ok {
		t.Error("flat struct with primitive fields should return ok=false")
	}
}

func TestIsInputObjectNullable(t *testing.T) {
	// Non-pointer wrapper field → required (NonNull) at SDL level.
	type valueWrapper = struct{ Input inputTestArgs }
	if isInputObjectNullable(reflect.TypeOf(valueWrapper{})) {
		t.Error("non-pointer wrapper field should NOT be nullable")
	}

	// Pointer wrapper field → nullable at SDL level.
	type pointerWrapper = struct{ Input *inputTestArgs }
	if !isInputObjectNullable(reflect.TypeOf(pointerWrapper{})) {
		t.Error("pointer wrapper field SHOULD be nullable")
	}

	// Non-struct argsType (e.g. a primitive) returns false defensively.
	if isInputObjectNullable(reflect.TypeOf(0)) {
		t.Error("non-struct argsType should return false")
	}
}

func TestDetectInputObject_StillMatchesPointerWrapper(t *testing.T) {
	// detectInputObject must keep dereferencing *Inner — pointer-ness only
	// affects nullability, not whether the wrapper qualifies as input-object.
	type pointerWrapper = struct{ Input *inputTestArgs }
	argsType := reflect.TypeOf(pointerWrapper{})

	name, inner, ok := detectInputObject(argsType)
	if !ok {
		t.Fatal("detectInputObject returned ok=false for *Inner wrapper")
	}
	if name != "input" {
		t.Errorf("arg name = %q; want input", name)
	}
	if inner != reflect.TypeOf(inputTestArgs{}) {
		t.Errorf("inner = %v; want inputTestArgs (dereferenced)", inner)
	}
}

func TestInspectHandler_ParamsWithAnonInputWrapper(t *testing.T) {
	fn := func(svc *testSvc, p Params[struct{ Input inputTestArgs }]) (*string, error) {
		return nil, nil
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatal(err)
	}
	if !sh.hasParams {
		t.Fatal("hasParams=false")
	}
	if !sh.hasArgs {
		t.Fatal("hasArgs=false; expected true because wrapper has 1 exported field")
	}
	// argsType should be the wrapper (struct{Input inputTestArgs}), not inputTestArgs itself.
	if sh.argsType.NumField() != 1 {
		t.Errorf("argsType.NumField=%d; want 1", sh.argsType.NumField())
	}
	if sh.argsType.Field(0).Name != "Input" {
		t.Errorf("outer field = %q; want Input", sh.argsType.Field(0).Name)
	}
	// detectInputObject on the wrapper should fire.
	name, _, ok := detectInputObject(sh.argsType)
	if !ok {
		t.Fatal("detectInputObject on wrapper returned ok=false")
	}
	if name != "input" {
		t.Errorf("detected name=%q", name)
	}
}
