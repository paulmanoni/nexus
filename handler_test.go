package nexus

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/graphql-go/graphql"
	"github.com/paulmanoni/nexus/v2/config"
	"github.com/paulmanoni/nexus/v2/graph"
	"github.com/paulmanoni/nexus/v2/httpx"
)

// These table tests pin the reflective handler core — inspectHandler's slot
// classification + return-arity rules, and callHandler's slot-filling +
// result/error extraction. This is zero-I/O code with the widest blast radius
// in the framework (every REST/GraphQL/WS handler flows through it), so the
// goal here is exhaustive coverage of signature permutations and their
// contracts, not happy-path smoke. Shared helpers testArgs/testSvc/testKey live
// in handler_test.go.

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
			name:       "result only, no error",
			fn:         func(*testSvc) *petResult { return nil },
			wantSlots:  []paramKind{paramDep},
			wantDeps:   []reflect.Type{reflect.TypeOf(&testSvc{})},
			wantReturn: tPet,
			wantErrIdx: -1, wantResIdx: 0, wantHasError: false,
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

type testArgs struct {
	Title string `graphql:"title,required" validate:"required,len=1|100"`
}

type testSvc struct{ *Service }

func TestInspectHandler_DetectsParamsT(t *testing.T) {
	fn := func(svc *testSvc, p Params[testArgs]) (*string, error) { return nil, nil }
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !sh.hasParams {
		t.Fatal("hasParams should be true")
	}
	if sh.paramsType == nil {
		t.Fatal("paramsType should be set")
	}
	if !sh.hasArgs {
		t.Fatal("hasArgs should be true when Params[T] has tagged fields")
	}
	if sh.argsType != reflect.TypeOf(testArgs{}) {
		t.Errorf("argsType = %v; want testArgs", sh.argsType)
	}
	if n := len(sh.depTypes); n != 1 {
		t.Errorf("depTypes len = %d; want 1 (*testSvc)", n)
	}
}

func TestInspectHandler_LegacyFlatArgsStillWorks(t *testing.T) {
	fn := func(svc *testSvc, ctx context.Context, a testArgs) (*string, error) { return nil, nil }
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if sh.hasParams {
		t.Error("hasParams should be false for legacy flat-args")
	}
	if !sh.hasArgs {
		t.Error("hasArgs should be true")
	}
	if !sh.hasCtx {
		t.Error("hasCtx should be true when context.Context is a param")
	}
}

func TestInspectHandler_RejectsDoubleParams(t *testing.T) {
	fn := func(svc *testSvc, a Params[testArgs], b Params[testArgs]) (*string, error) { return nil, nil }
	if _, err := inspectHandler(fn); err == nil {
		t.Fatal("expected error for two Params[T] params")
	}
}

func TestInspectHandler_EmptyParamsStructHasNoArgs(t *testing.T) {
	fn := func(svc *testSvc, p Params[struct{}]) (*string, error) { return nil, nil }
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !sh.hasParams {
		t.Fatal("hasParams should be true")
	}
	if sh.hasArgs {
		t.Error("hasArgs should be false for Params[struct{}]")
	}
}

func TestCallHandler_FillsParamsBundle(t *testing.T) {
	var got Params[testArgs]
	var gotDep *testSvc
	fn := func(svc *testSvc, p Params[testArgs]) (*string, error) {
		got = p
		gotDep = svc
		return nil, nil
	}
	sh, err := inspectHandler(fn)
	if err != nil {
		t.Fatal(err)
	}
	svc := &testSvc{Service: &Service{name: "test"}}
	args := reflect.ValueOf(testArgs{Title: "hi"})
	info := graphql.ResolveInfo{FieldName: "testOp"}
	ctx := context.WithValue(context.Background(), testKey{}, "sentinel")

	_, callErr := sh.callHandler(
		callInput{Ctx: ctx, Source: "parent", Info: info},
		[]reflect.Value{reflect.ValueOf(svc)},
		args,
	)
	if callErr != nil {
		t.Fatalf("call: %v", callErr)
	}
	if got.Context == nil || got.Context.Value(testKey{}) != "sentinel" {
		t.Errorf("Context = %v; want one carrying the sentinel value", got.Context)
	}
	if got.Args.Title != "hi" {
		t.Errorf("Args.Title = %q; want hi", got.Args.Title)
	}
	if got.Source != "parent" {
		t.Errorf("Source = %v; want parent", got.Source)
	}
	if got.Info.FieldName != "testOp" {
		t.Errorf("Info.FieldName = %q", got.Info.FieldName)
	}
	if gotDep != svc {
		t.Error("dep not passed through")
	}
}

type testKey struct{}

// Service-method handlers: AsQuery/AsMutation/AsRest accept a method
// expression ((*Svc).Method) or a bound method value (svc.Method) directly —
// the receiver is a DI-injected dep, ctx fills from the request, a trailing
// struct param is the args container, and the op name derives from the
// method name. This is the wrapper-free registration shape; these tests pin
// it as a supported contract, not an accident of the reflector.

type probeSvc struct{ prefix string }

type probeArgs struct {
	Name string `json:"name" query:"name"`
}

type probeOut struct {
	Greeting string `json:"greeting"`
}

func (s *probeSvc) GreetUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: s.prefix + a.Name}, nil
}

func (s *probeSvc) RenameUser(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: "renamed " + a.Name}, nil
}

// ListGreetings has no args struct at all — just the receiver and ctx.
func (s *probeSvc) ListGreetings(ctx context.Context) ([]probeOut, error) {
	return []probeOut{{Greeting: s.prefix + "all"}}, nil
}

func postGraphQL(t *testing.T, app *App, query string) string {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/graphql", strings.NewReader(`{"query":`+jsonStr(query)+`}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	return w.Body.String()
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestMethodExpressionHandlers(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		Supply(&probeSvc{prefix: "hi "}),
		AsQuery((*probeSvc).GreetUser),
		AsQuery((*probeSvc).ListGreetings),
		AsMutation((*probeSvc).RenameUser),
		AsRest("GET", "/greet", (*probeSvc).GreetUser),
		AsRest("POST", "/rename", (*probeSvc).RenameUser),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ greetUser(name: "ada") { greeting } }`); !strings.Contains(got, "hi ada") {
		t.Fatalf("greetUser = %s", got)
	}
	if got := postGraphQL(t, app, `{ listGreetings { greeting } }`); !strings.Contains(got, "hi all") {
		t.Fatalf("listGreetings = %s", got)
	}
	if got := postGraphQL(t, app, `mutation { renameUser(name: "bo") { greeting } }`); !strings.Contains(got, "renamed bo") {
		t.Fatalf("renameUser = %s", got)
	}

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest("GET", "/greet?name=zed", nil))
	if !strings.Contains(w.Body.String(), "hi zed") {
		t.Fatalf("GET /greet = %s", w.Body.String())
	}

	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/rename", strings.NewReader(`{"name":"kim"}`))
	req.Header.Set("Content-Type", "application/json")
	app.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "renamed kim") {
		t.Fatalf("POST /rename = %s", w.Body.String())
	}
}

// A bound method value must register under the same op name as the method
// expression — the runtime decorates it "GreetUser-fm" and the name
// extraction strips the wrapper suffix.
func TestBoundMethodValueHandler(t *testing.T) {
	svc := &probeSvc{prefix: "yo "}
	app, stop, err := InProcess(config.Runtime{},
		AsQuery(svc.GreetUser),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ greetUser(name: "lin") { greeting } }`); !strings.Contains(got, "yo lin") {
		t.Fatalf("bound greetUser = %s", got)
	}
}

// The plain free-function shape — ctx first, args struct last, no Params[T]
// — is the same contract the method forms ride on.
func plainGreet(ctx context.Context, a probeArgs) (*probeOut, error) {
	return &probeOut{Greeting: "plain " + a.Name}, nil
}

func TestPlainFuncHandler(t *testing.T) {
	app, stop, err := InProcess(config.Runtime{},
		AsQuery(plainGreet),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stop(context.Background()) }()

	if got := postGraphQL(t, app, `{ plainGreet(name: "mia") { greeting } }`); !strings.Contains(got, "plain mia") {
		t.Fatalf("plainGreet = %s", got)
	}
}

// GraphQL list args arrive as []interface{} regardless of the declared element
// type. The flat-args bind path must coerce them into the destination's typed
// slice (e.g. []string, []int) instead of failing with "cannot assign".

func TestBindGqlArgs_StringSlice(t *testing.T) {
	var args struct {
		InterviewUids []string `graphql:"interviewUids"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"interviewUids": []interface{}{"a", "b", "c"},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !reflect.DeepEqual(args.InterviewUids, []string{"a", "b", "c"}) {
		t.Fatalf("got %#v", args.InterviewUids)
	}
}

func TestBindGqlArgs_IntSlice(t *testing.T) {
	var args struct {
		IDs []int `graphql:"ids"`
	}
	// graphql-go decodes Int as int; mirror that here.
	if err := bindGqlArgs(&args, map[string]any{
		"ids": []interface{}{1, 2, 3},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if !reflect.DeepEqual(args.IDs, []int{1, 2, 3}) {
		t.Fatalf("got %#v", args.IDs)
	}
}

func TestBindGqlArgs_EmptySlice(t *testing.T) {
	var args struct {
		Uids []string `graphql:"uids"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"uids": []interface{}{},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if len(args.Uids) != 0 {
		t.Fatalf("expected empty slice, got %#v", args.Uids)
	}
}

type innerInput struct {
	Foo string `graphql:"foo"`
	N   int    `graphql:"n"`
}

func TestBindGqlArgs_PointerStruct(t *testing.T) {
	var args struct {
		Inner *innerInput `graphql:"inner"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"inner": map[string]any{"foo": "x", "n": 7},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if args.Inner == nil || args.Inner.Foo != "x" || args.Inner.N != 7 {
		t.Fatalf("got %#v", args.Inner)
	}
}

func TestBindGqlArgs_ValueStruct(t *testing.T) {
	var args struct {
		Inner innerInput `graphql:"inner"`
	}
	if err := bindGqlArgs(&args, map[string]any{
		"inner": map[string]any{"foo": "y", "n": 3},
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if args.Inner.Foo != "y" || args.Inner.N != 3 {
		t.Fatalf("got %#v", args.Inner)
	}
}

func TestBindGqlArgs_ScalarDestRejectsMap(t *testing.T) {
	// A scalar destination given a struct/map value should still fail
	// rather than silently produce garbage.
	var args struct {
		Name *innerInput `graphql:"name"`
	}
	err := bindGqlArgs(&args, map[string]any{
		"name": 42, // not a map, not a string
	})
	if err == nil {
		t.Fatal("expected error for incompatible types")
	}
}

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

// Rebuild what asGqlField does to a resolver, then verify the assembled
// r.args entry actually carries the input-object type rather than flat
// fields. This locks in the integration between detectInputObject and
// go-graph's WithInputObject.
func TestApplyArgsFromStruct_BuildsInputObject(t *testing.T) {
	type Response struct {
		Message string `json:"message"`
	}

	argsType := reflect.TypeOf(inputTestWrapper{})
	r := graph.NewResolverFromType("createThing", reflect.TypeOf(Response{}))

	inputName := applyArgsFromStruct(r, argsType)
	if inputName != "input" {
		t.Fatalf("applyArgsFromStruct returned %q; want input", inputName)
	}

	// Build the schema and run an introspection query to see what
	// graphql-go actually ended up with.
	r.WithRawResolver(func(p graph.ResolveParams) (any, error) {
		return &Response{Message: "ok"}, nil
	})
	m := r.BuildMutation()

	schema, err := graph.NewSchemaBuilder(graph.SchemaBuilderParams{
		QueryFields: []graph.QueryField{
			graph.NewResolverFromType("ping", reflect.TypeOf(Response{})).
				WithRawResolver(func(p graph.ResolveParams) (any, error) {
					return &Response{}, nil
				}).BuildQuery(),
		},
		MutationFields: []graph.MutationField{m},
	}).Build()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}

	// Ask the schema about the mutation field's args.
	res := graphql.Do(graphql.Params{
		Schema:        schema,
		RequestString: `{ __type(name: "Mutation") { fields { name args { name type { name kind ofType { name kind } } } } } }`,
	})
	if len(res.Errors) > 0 {
		t.Fatalf("introspect: %v", res.Errors)
	}

	// Walk the introspection result to find createThing's args.
	data, _ := res.Data.(map[string]interface{})
	typ, _ := data["__type"].(map[string]interface{})
	fields, _ := typ["fields"].([]interface{})
	var argsSlice []interface{}
	for _, f := range fields {
		fm := f.(map[string]interface{})
		if fm["name"] == "createThing" {
			argsSlice, _ = fm["args"].([]interface{})
			break
		}
	}
	if argsSlice == nil {
		t.Fatal("createThing not found in Mutation fields")
	}
	if len(argsSlice) != 1 {
		names := make([]string, len(argsSlice))
		for i, a := range argsSlice {
			names[i] = a.(map[string]interface{})["name"].(string)
		}
		t.Fatalf("expected 1 arg (input-object), got %d: %v", len(argsSlice), names)
	}
	a0 := argsSlice[0].(map[string]interface{})
	if a0["name"] != "input" {
		t.Errorf("arg name = %v; want input", a0["name"])
	}
}
