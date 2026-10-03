package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestMigrateV2_Errors(t *testing.T) {
	src := `package users

import (
	"context"
	"errors"
	"net/http"

	nx "github.com/paulmanoni/nexus"
)

func Options(err error) nx.Option { return nx.Error(err) }

func (s *Svc) Create(ctx context.Context, in Input) (*User, error) {
	errs := nx.NewErrors()
	if in.Email == "" {
		errs.Field("email", "required")
	}
	if errs.Any() {
		return nil, errs
	}
	if !s.allowed(ctx) {
		return nil, nx.ErrForbidden
	}
	return nil, nil
}

func status(err error) int {
	var ve *nx.Errors
	if errors.As(err, &ve) || errors.Is(err, nx.ErrForbidden) {
		return 1
	}
	if s, ok := nx.MapCRUDError(err); ok {
		return s
	}
	return http.StatusOK
}
`
	var goRules []migrateRule
	for _, r := range migrateV2Rules {
		if r.Applies("x.go") {
			goRules = append(goRules, r)
		}
	}
	res, err := migrateFile("users.go", []byte(src), goRules)
	if err != nil || res == nil {
		t.Fatalf("migrate: %v %v", res, err)
	}
	out := string(res.Out)
	for _, want := range []string{
		`return nx.FailBoot(err)`,
		`errs := nx.Invalid()`,
		`errs.Field("email", "required")`,
		`if errs.Any() {`,
		`return nil, nx.Forbidden`,
		`var ve *nx.Error`,
		`errors.Is(err, nx.Forbidden)`,
		`// TODO(nexus v2): nexus.MapCRUDError is gone`,
		`"github.com/paulmanoni/nexus/v2"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if _, err := parser.ParseFile(token.NewFileSet(), "", res.Out, parser.AllErrors); err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	again, err := migrateFile("users.go", res.Out, goRules)
	if err != nil || again != nil {
		t.Fatalf("second run changed the file: %v %v", again, err)
	}
}
