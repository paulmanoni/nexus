package nexus

import (
	"context"
	"reflect"
	"testing"

	"github.com/graphql-go/graphql"
)

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
