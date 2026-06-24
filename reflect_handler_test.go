package nexus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/httpx"
)

// These table tests pin the reflective handler core — inspectHandler's slot
// classification + return-arity rules, and callHandler's slot-filling +
// result/error extraction. This is zero-I/O code with the widest blast radius
// in the framework (every REST/GraphQL/WS handler flows through it), so the
// goal here is exhaustive coverage of signature permutations and their
// contracts, not happy-path smoke. Shared helpers testArgs/testSvc/testKey live
// in reflect_params_test.go.

// --- helper types used only by these tests ---

type petResult struct{ Name string }

type otherDep struct{ id int } //nolint:unused // referenced via *otherDep in signatures

type depStruct struct{ X int } // a value-typed (non-pointer) DI dependency

func slotKinds(sh handlerShape) []paramKind {
	ks := make([]paramKind, len(sh.slots))
	for i, s := range sh.slots {
		ks[i] = s.kind
	}
	return ks
}

func kindName(k paramKind) string {
	switch k {
	case paramDep:
		return "dep"
	case paramCtx:
		return "ctx"
	case paramArgs:
		return "args"
	case paramParams:
		return "params"
	case paramGinCtx:
		return "ginctx"
	case paramWS:
		return "ws"
	default:
		return "?"
	}
}

func kindNames(ks []paramKind) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = kindName(k)
	}
	return out
}

// TestInspectHandler_Classification exhaustively covers how each parameter
// position is classified and how flags/argsType/return shape are derived for
// every valid handler permutation.
func TestInspectHandler_Classification(t *testing.T) {
	tArgs := reflect.TypeOf(testArgs{})
	tPet := reflect.TypeOf(&petResult{})

	tests := []struct {
		name         string
		fn           any
		wantSlots    []paramKind
		wantDeps     []reflect.Type
		wantArgs     bool
		wantCtx      bool
		wantParams   bool
		wantArgsType reflect.Type // nil = don't assert
		wantReturn   reflect.Type // nil = no result return
		wantHasError bool
		wantErrIdx   int
		wantResIdx   int
	}{
		{
			name:       "error only, no params",
			fn:         func() error { return nil },
			wantSlots:  []paramKind{},
			wantDeps:   nil,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:       "no returns at all",
			fn:         func(*testSvc) {},
			wantSlots:  []paramKind{paramDep},
			wantDeps:   []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantErrIdx: -1, wantResIdx: -1, wantHasError: false,
		},
		{
			name:         "result only, no error",
			fn:           func(*testSvc) *petResult { return nil },
			wantSlots:    []paramKind{paramDep},
			wantDeps:     []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantReturn:   tPet,
			wantErrIdx:   -1, wantResIdx: 0, wantHasError: false,
		},
		{
			name:       "dep + (T, error)",
			fn:         func(*testSvc) (*petResult, error) { return nil, nil },
			wantSlots:  []paramKind{paramDep},
			wantDeps:   []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantReturn: tPet,
			wantErrIdx: 1, wantResIdx: 0, wantHasError: true,
		},
		{
			name:         "legacy flat trailing struct -> args",
			fn:           func(*testSvc, testArgs) (*petResult, error) { return nil, nil },
			wantSlots:    []paramKind{paramDep, paramArgs},
			wantDeps:     []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantArgs:     true,
			wantArgsType: tArgs,
			wantReturn:   tPet,
			wantErrIdx:   1, wantResIdx: 0, wantHasError: true,
		},
		{
			name:         "ctx + legacy args",
			fn:           func(context.Context, testArgs) error { return nil },
			wantSlots:    []paramKind{paramCtx, paramArgs},
			wantDeps:     nil,
			wantArgs:     true,
			wantCtx:      true,
			wantArgsType: tArgs,
			wantErrIdx:   0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:         "Params[T]",
			fn:           func(*testSvc, Params[testArgs]) (*petResult, error) { return nil, nil },
			wantSlots:    []paramKind{paramDep, paramParams},
			wantDeps:     []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantArgs:     true,
			wantParams:   true,
			wantArgsType: tArgs,
			wantReturn:   tPet,
			wantErrIdx:   1, wantResIdx: 0, wantHasError: true,
		},
		{
			name:         "Params[T] in the middle, deps on both sides",
			fn:           func(*testSvc, Params[testArgs], *otherDep) (*petResult, error) { return nil, nil },
			wantSlots:    []paramKind{paramDep, paramParams, paramDep},
			wantDeps:     []reflect.Type{reflect.TypeOf(&testSvc{}), reflect.TypeOf(&otherDep{})},
			wantArgs:     true,
			wantParams:   true,
			wantArgsType: tArgs,
			wantReturn:   tPet,
			wantErrIdx:   1, wantResIdx: 0, wantHasError: true,
		},
		{
			name:       "Params[struct{}] has no args",
			fn:         func(Params[struct{}]) error { return nil },
			wantSlots:  []paramKind{paramParams},
			wantParams: true,
			wantArgs:   false,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:         "trailing struct is a DEP (not args) when Params[T] present",
			fn:           func(Params[testArgs], depStruct) error { return nil },
			wantSlots:    []paramKind{paramParams, paramDep},
			wantDeps:     []reflect.Type{reflect.TypeOf(depStruct{})},
			wantArgs:     true, // from Params[testArgs]
			wantParams:   true,
			wantArgsType: tArgs,
			wantErrIdx:   0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:       "non-struct trailing param is a dep, not args",
			fn:         func(string) error { return nil },
			wantSlots:  []paramKind{paramDep},
			wantDeps:   []reflect.Type{reflect.TypeOf("")},
			wantArgs:   false,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:       "pointer trailing param is a dep, not args",
			fn:         func(*petResult) error { return nil },
			wantSlots:  []paramKind{paramDep},
			wantDeps:   []reflect.Type{tPet},
			wantArgs:   false,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:       "*httpx.Ctx + Params[T]",
			fn:         func(*httpx.Ctx, Params[testArgs]) error { return nil },
			wantSlots:  []paramKind{paramGinCtx, paramParams},
			wantParams: true, wantArgs: true, wantArgsType: tArgs,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:       "*WSSession + Params[T]",
			fn:         func(*WSSession, Params[testArgs]) error { return nil },
			wantSlots:  []paramKind{paramWS, paramParams},
			wantParams: true, wantArgs: true, wantArgsType: tArgs,
			wantErrIdx: 0, wantResIdx: -1, wantHasError: true,
		},
		{
			name:         "ctx + ginctx + ws + params, all special slots",
			fn:           func(context.Context, *httpx.Ctx, *WSSession, Params[testArgs]) (*petResult, error) { return nil, nil },
			wantSlots:    []paramKind{paramCtx, paramGinCtx, paramWS, paramParams},
			wantDeps:     nil,
			wantCtx:      true,
			wantParams:   true,
			wantArgs:     true,
			wantArgsType: tArgs,
			wantReturn:   tPet,
			wantErrIdx:   1, wantResIdx: 0, wantHasError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh, err := inspectHandler(tt.fn)
			if err != nil {
				t.Fatalf("inspectHandler: unexpected error: %v", err)
			}
			if got := slotKinds(sh); !reflect.DeepEqual(got, tt.wantSlots) {
				t.Errorf("slot kinds = %v; want %v", kindNames(got), kindNames(tt.wantSlots))
			}
			if !reflect.DeepEqual(sh.depTypes, tt.wantDeps) {
				t.Errorf("depTypes = %v; want %v", sh.depTypes, tt.wantDeps)
			}
			if sh.hasArgs != tt.wantArgs {
				t.Errorf("hasArgs = %v; want %v", sh.hasArgs, tt.wantArgs)
			}
			if sh.hasCtx != tt.wantCtx {
				t.Errorf("hasCtx = %v; want %v", sh.hasCtx, tt.wantCtx)
			}
			if sh.hasParams != tt.wantParams {
				t.Errorf("hasParams = %v; want %v", sh.hasParams, tt.wantParams)
			}
			if tt.wantArgsType != nil && sh.argsType != tt.wantArgsType {
				t.Errorf("argsType = %v; want %v", sh.argsType, tt.wantArgsType)
			}
			if sh.returnType != tt.wantReturn {
				t.Errorf("returnType = %v; want %v", sh.returnType, tt.wantReturn)
			}
			if sh.hasError != tt.wantHasError {
				t.Errorf("hasError = %v; want %v", sh.hasError, tt.wantHasError)
			}
			if sh.errorIdx != tt.wantErrIdx {
				t.Errorf("errorIdx = %d; want %d", sh.errorIdx, tt.wantErrIdx)
			}
			if sh.resultIdx != tt.wantResIdx {
				t.Errorf("resultIdx = %d; want %d", sh.resultIdx, tt.wantResIdx)
			}
			// hasParams implies paramsType set; and vice-versa.
			if sh.hasParams != (sh.paramsType != nil) {
				t.Errorf("hasParams=%v but paramsType=%v (must agree)", sh.hasParams, sh.paramsType)
			}
		})
	}
}

// TestInspectHandler_Errors covers every rejected shape and asserts the error
// message is descriptive (it surfaces at boot, so the substring matters).
func TestInspectHandler_Errors(t *testing.T) {
	tests := []struct {
		name       string
		fn         any
		wantSubstr string
	}{
		{"nil handler", nil, "nil"},
		{"not a func", 42, "must be a func"},
		{"three returns", func() (int, int, error) { return 0, 0, nil }, "expected 0..2 returns"},
		{"second return not error", func() (int, int) { return 0, 0 }, "second return must be error"},
		{"two Params[T]", func(Params[testArgs], Params[testArgs]) error { return nil }, "more than one Params"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := inspectHandler(tt.fn)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error = %q; want substring %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

// TestReturnElementType pins the contract: strip pointer layers only. The slice
// case is intentionally left to NewResolverFromType downstream, so []*T stays
// []*T here (the doc example "[]*Pet -> Pet" describes the end-to-end registry
// keying, not this function in isolation).
func TestReturnElementType(t *testing.T) {
	pet := reflect.TypeOf(petResult{})
	tests := []struct {
		name string
		fn   any
		want reflect.Type
	}{
		{"no result return -> nil", func() error { return nil }, nil},
		{"value T -> T", func() petResult { return petResult{} }, pet},
		{"*T -> T", func() *petResult { return nil }, pet},
		{"**T -> T", func() **petResult { return nil }, pet},
		{"[]*T stays []*T", func() []*petResult { return nil }, reflect.TypeOf([]*petResult(nil))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sh, err := inspectHandler(tt.fn)
			if err != nil {
				t.Fatalf("inspect: %v", err)
			}
			if got := sh.returnElementType(); got != tt.want {
				t.Errorf("returnElementType() = %v; want %v", got, tt.want)
			}
		})
	}
}

// --- callHandler invocation semantics ---

func mustInspect(t *testing.T, fn any) handlerShape {
	t.Helper()
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	return sh
}

func TestCallHandler_DepsAndLegacyArgsAndCtx(t *testing.T) {
	var gotSvc *testSvc
	var gotCtx context.Context
	var gotArgs testArgs
	fn := func(svc *testSvc, ctx context.Context, a testArgs) error {
		gotSvc, gotCtx, gotArgs = svc, ctx, a
		return nil
	}
	sh := mustInspect(t, fn)
	svc := &testSvc{Service: &Service{name: "x"}}
	_, err := sh.callHandler(
		callInput{Ctx: context.Background()},
		[]reflect.Value{reflect.ValueOf(svc)},
		reflect.ValueOf(testArgs{Title: "hi"}),
	)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotSvc != svc {
		t.Error("dep not passed through")
	}
	if gotCtx == nil {
		t.Error("ctx slot not filled")
	}
	if gotArgs.Title != "hi" {
		t.Errorf("args.Title = %q; want hi", gotArgs.Title)
	}
}

func TestCallHandler_NilCtxDefaultsToBackground(t *testing.T) {
	var gotCtx context.Context
	fn := func(ctx context.Context) error { gotCtx = ctx; return nil }
	sh := mustInspect(t, fn)
	if _, err := sh.callHandler(callInput{Ctx: nil}, nil, reflect.Value{}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if gotCtx == nil {
		t.Fatal("ctx should default to context.Background(), got nil")
	}
}

func TestCallHandler_GinCtxNilVsProvided(t *testing.T) {
	var got *httpx.Ctx
	fn := func(c *httpx.Ctx) error { got = c; return nil }
	sh := mustInspect(t, fn)

	// Not the REST transport: a typed nil is handed in so handlers can guard.
	if _, err := sh.callHandler(callInput{}, nil, reflect.Value{}); err != nil {
		t.Fatalf("call (absent): %v", err)
	}
	if got != nil {
		t.Errorf("GinCtx absent: got %v; want typed nil", got)
	}

	// REST transport: the concrete *httpx.Ctx is threaded through.
	c := &httpx.Ctx{}
	if _, err := sh.callHandler(callInput{GinCtx: c}, nil, reflect.Value{}); err != nil {
		t.Fatalf("call (present): %v", err)
	}
	if got != c {
		t.Errorf("GinCtx present: got %v; want %v", got, c)
	}
}

func TestCallHandler_WSNilVsProvided(t *testing.T) {
	var got *WSSession
	fn := func(s *WSSession) error { got = s; return nil }
	sh := mustInspect(t, fn)

	if _, err := sh.callHandler(callInput{}, nil, reflect.Value{}); err != nil {
		t.Fatalf("call (absent): %v", err)
	}
	if got != nil {
		t.Errorf("WS absent: got %v; want typed nil", got)
	}

	sess := &WSSession{}
	if _, err := sh.callHandler(callInput{WS: sess}, nil, reflect.Value{}); err != nil {
		t.Fatalf("call (present): %v", err)
	}
	if got != sess {
		t.Errorf("WS present: got %v; want %v", got, sess)
	}
}

func TestCallHandler_ResultExtraction(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("nil pointer result collapses to nil", func(t *testing.T) {
		sh := mustInspect(t, func() (*petResult, error) { return nil, nil })
		res, err := sh.callHandler(callInput{}, nil, reflect.Value{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if res != nil {
			t.Errorf("res = %#v; want nil (typed-nil pointer must collapse)", res)
		}
	})

	t.Run("non-nil result returned as interface", func(t *testing.T) {
		want := &petResult{Name: "rex"}
		sh := mustInspect(t, func() (*petResult, error) { return want, nil })
		res, err := sh.callHandler(callInput{}, nil, reflect.Value{})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		got, ok := res.(*petResult)
		if !ok || got != want {
			t.Errorf("res = %#v; want %#v", res, want)
		}
	})

	t.Run("error-only handler propagates error", func(t *testing.T) {
		sh := mustInspect(t, func() error { return sentinel })
		res, err := sh.callHandler(callInput{}, nil, reflect.Value{})
		if res != nil {
			t.Errorf("res = %#v; want nil", res)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v; want sentinel", err)
		}
	})

	t.Run("nil error interface yields no error", func(t *testing.T) {
		sh := mustInspect(t, func() error { return nil })
		_, err := sh.callHandler(callInput{}, nil, reflect.Value{})
		if err != nil {
			t.Errorf("err = %v; want nil", err)
		}
	})

	t.Run("result + error: error returned, nil result collapses", func(t *testing.T) {
		sh := mustInspect(t, func() (*petResult, error) { return nil, sentinel })
		res, err := sh.callHandler(callInput{}, nil, reflect.Value{})
		if res != nil {
			t.Errorf("res = %#v; want nil", res)
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v; want sentinel", err)
		}
	})
}
