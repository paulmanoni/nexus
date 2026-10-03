package main

import (
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper for the resolver tests.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportsOfFileBlankAndDot(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.go")
	writeFile(t, f, `package x

import (
	_ "github.com/acme/inertia"
	. "fmt"
	al "github.com/acme/aliased"
	"github.com/acme/plain"
)
`)
	imps, err := importsOfFile(f)
	if err != nil {
		t.Fatal(err)
	}
	// Blank import recorded under its path tail as a PLAIN import.
	if got := imps["inertia"]; got != `"github.com/acme/inertia"` {
		t.Errorf("blank import: got %q", got)
	}
	// Dot import dropped (no usable selector).
	if _, ok := imps["fmt"]; ok {
		t.Errorf("dot import should be skipped, got %q", imps["fmt"])
	}
	// Alias preserved.
	if got := imps["al"]; got != `al "github.com/acme/aliased"` {
		t.Errorf("alias: got %q", got)
	}
	// Plain import keyed by path tail.
	if got := imps["plain"]; got != `"github.com/acme/plain"` {
		t.Errorf("plain: got %q", got)
	}
}

func TestImportLineFor(t *testing.T) {
	if got := importLineFor("inertia", "github.com/x/inertia"); got != `"github.com/x/inertia"` {
		t.Errorf("tail==sel should be plain, got %q", got)
	}
	if got := importLineFor("inertia", "github.com/x/inertiapkg"); got != `inertia "github.com/x/inertiapkg"` {
		t.Errorf("tail!=sel should alias, got %q", got)
	}
}

func TestResolveLayer1FileImport(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "h.go")
	writeFile(t, f, "package h\n\nimport \"github.com/acme/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(f, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/acme/inertia"` {
		t.Errorf("layer1: got %q", got)
	}
}

func TestResolveLayer2SiblingImport(t *testing.T) {
	dir := t.TempDir()
	// The annotated file does NOT import inertia...
	annotated := filepath.Join(dir, "academic_level.go")
	writeFile(t, annotated, "package settings\n")
	// ...but a sibling file in the same package does.
	writeFile(t, filepath.Join(dir, "module.go"), "package settings\n\nimport \"github.com/acme/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(annotated, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/acme/inertia"` {
		t.Errorf("layer2: got %q", got)
	}
}

func TestResolveLayer4TomlHint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "h.go")
	writeFile(t, f, "package h\n")
	writeFile(t, filepath.Join(dir, "nexus.toml"), "[decorators.imports]\ninertia = \"github.com/custom/inertia\"\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(f, "inertia")
	if err != nil {
		t.Fatal(err)
	}
	if got != `"github.com/custom/inertia"` {
		t.Errorf("layer4: got %q", got)
	}
}

func TestResolveLayer3ModuleGraph(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "foo", "foo.go"), "package foo\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	got, err := r.resolve(annotated, "foo")
	if err != nil {
		t.Fatalf("layer3: %v", err)
	}
	if got != `"example.com/m/foo"` {
		t.Errorf("layer3: got %q", got)
	}
}

func TestResolveLayer3Ambiguous(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "a", "a.go"), "package foo\n")
	writeFile(t, filepath.Join(dir, "b", "b.go"), "package foo\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	_, err := r.resolve(annotated, "foo")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestResolveNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")
	r := newSelectorResolver(dir)
	_, err := r.resolve(annotated, "nope")
	if err == nil || !strings.Contains(err.Error(), "not a dependency") {
		t.Fatalf("expected not-a-dependency error, got %v", err)
	}
}

func TestNearHandlerKeyword(t *testing.T) {
	cases := map[string]struct {
		want  string
		close bool
	}{
		"Rest":       {"rest", true},  // case typo
		"quer":       {"query", true}, // dropped letter
		"queyr":      {"query", true}, // transposition
		"mutations":  {"mutation", true},
		"providr":    {"provide", true},
		"test":       {"", false}, // different first letter — another tool's
		"user":       {"use", true},
		"deprecated": {"", false},
		"wrap":       {"", false},
		"decorate":   {"", false},
	}
	for kw, c := range cases {
		got, close := nearHandlerKeyword(kw)
		if close != c.close || (close && got != c.want) {
			t.Errorf("nearHandlerKeyword(%q) = (%q, %v), want (%q, %v)", kw, got, close, c.want, c.close)
		}
	}
}

// TestScanHandlerSites_Strictness covers the scan-level guarantees end to end:
// a lowercase //nexus:rest method is normalised to the HTTP verb, a typo'd keyword
// is a positioned error with a suggestion, a malformed directive reports the
// annotation's own file:line, and genuinely foreign keywords stay ignored.
func TestScanHandlerSites_Strictness(t *testing.T) {
	// Lowercase method normalises; the route registers as GET.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `package h

//nexus:rest get /users
func NewList() {}
`)
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(results) != 1 || !strings.Contains(string(results[0].Content), `nexus.AsRest("GET", "/users"`) {
		t.Fatalf("lowercase method not normalised to GET:\n%v", results)
	}

	// A near-miss keyword is an error with a suggestion, not a silent no-op.
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "h.go"), `package h

//nexus:quer
func NewList() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "did you mean //nexus:query") {
		t.Fatalf("typo should suggest //nexus:query, got: %v", err)
	}
	if !strings.Contains(err.Error(), "h.go:3:") {
		t.Fatalf("typo error should carry file:line, got: %v", err)
	}

	// A malformed known directive reports the annotation's file:line.
	dir3 := t.TempDir()
	writeFile(t, filepath.Join(dir3, "h.go"), `package h

//nexus:rest GET users
func NewList() {}
`)
	_, err = scanHandlerSites(dir3, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "h.go:3:") || !strings.Contains(err.Error(), `must start with "/"`) {
		t.Fatalf("malformed //nexus:rest should carry file:line and the rule, got: %v", err)
	}

	// The nexus: namespace is nexus's own: an unknown keyword is an error
	// even when it is no near miss.
	dir4 := t.TempDir()
	writeFile(t, filepath.Join(dir4, "h.go"), `package h

//nexus:deprecated
//nexus:query
func NewList() {}
`)
	_, err = scanHandlerSites(dir4, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "h.go:3:") || !strings.Contains(err.Error(), "unknown annotation //nexus:deprecated") {
		t.Fatalf("unknown nexus: keyword should be a positioned error, got: %v", err)
	}

	// Other tools' @-annotations stay ignored (coexistence).
	dir5 := t.TempDir()
	writeFile(t, filepath.Join(dir5, "h.go"), `package h

// List lists.
//
// @Summary List things
// @deprecated
//nexus:query
func NewList() {}
`)
	if _, err := scanHandlerSites(dir5, "nexus_handlers_gen.go"); err != nil {
		t.Fatalf("foreign @-annotation must not error: %v", err)
	}
}

// TestScanHandlerSites_LegacySpelling: v2 reads only //nexus: directives. A
// nexus keyword in the v1 //@ spelling — on a function, the package doc or a
// type, unspaced or gofmt's "// @" — and a spaced "// nexus:" are file:line
// errors; the v1 ones point at `nexus migrate v2`.
func TestScanHandlerSites_LegacySpelling(t *testing.T) {
	cases := map[string]struct{ src, want string }{
		"func": {"package h\n\n//@rest GET /users\nfunc NewList() {}\n",
			"h.go:3: //@rest is the nexus v1 annotation spelling"},
		"gofmt-spaced": {"package h\n\n// List lists.\n//\n// @query\nfunc NewList() {}\n",
			"h.go:5: //@query is the nexus v1 annotation spelling"},
		"custom": {"package h\n\n//@inertia.Page GET /login Login\nfunc NewLogin() {}\n",
			"h.go:3: //@inertia.Page is the nexus v1 annotation spelling"},
		"package": {"// Package h.\n//\n//@module billing\npackage h\n\n//nexus:query\nfunc NewList() {}\n",
			"h.go:3: //@module is the nexus v1 annotation spelling"},
		"type": {"package h\n\n//@controller /users\ntype UsersController struct{}\n\n//nexus:page GET /\nfunc (c *UsersController) Index() {}\n",
			"h.go:3: //@controller is the nexus v1 annotation spelling"},
		"spaced-directive": {"package h\n\n// nexus:rest GET /users\nfunc NewList() {}\n",
			`h.go:3: "// nexus:rest GET /users" is not a Go directive`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "h.go"), tc.src)
			_, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got: %v", tc.want, err)
			}
			if !strings.Contains(tc.want, "not a Go directive") && !strings.Contains(err.Error(), "nexus migrate v2") {
				t.Fatalf("v1 spelling error should point at nexus migrate v2: %v", err)
			}
		})
	}
}

// TestScanHandlerSites_GofmtPlacement: gofmt moves //nexus: directives below
// the doc prose; the scan reads them wherever they sit in the doc comment.
func TestScanHandlerSites_GofmtPlacement(t *testing.T) {
	src := []byte(`package h

//nexus:rest GET /users/:id
//nexus:auth Required
// GetUser returns one user.
func NewGetUser() {}
`)
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatal(err)
	}
	want := "// GetUser returns one user.\n//\n//nexus:rest GET /users/:id\n//nexus:auth Required\nfunc NewGetUser"
	if !strings.Contains(string(formatted), want) {
		t.Fatalf("gofmt did not keep the directives verbatim below the prose:\n%s", formatted)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), string(formatted))
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(results) != 1 || !strings.Contains(string(results[0].Content), `nexus.AsRest("GET", "/users/:id", NewGetUser, auth.Required())`) {
		t.Fatalf("directives below the prose not registered:\n%v", results)
	}
}

// TestScanHandlerSites_InertiaPage: the //nexus:inertia.Page decorator end to end —
// bare tokens resolve, normalise, and emit a quoted registrar call; a bad verb
// is a positioned error at the annotation.
func TestScanHandlerSites_InertiaPage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `package h

import _ "github.com/paulmanoni/nexus/v2/extension/inertia"

//nexus:inertia.Page get,post /login Login
func NewLogin() {}
`)
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(results) != 1 || !strings.Contains(string(results[0].Content), `inertia.Page("GET,POST", "/login", "Login", NewLogin)`) {
		t.Fatalf("bare-token inertia.Page not normalised:\n%v", results)
	}

	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "h.go"), `package h

import _ "github.com/paulmanoni/nexus/v2/extension/inertia"

//nexus:inertia.Page FETCH /login Login
func NewLogin() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "not an HTTP method") || !strings.Contains(err.Error(), "h.go:5:") {
		t.Fatalf("bad verb should be a positioned annotation error, got: %v", err)
	}
}

// TestScanHandlerSites_PackageDirectives: //nexus:module and //nexus:path on the
// package doc comment flow through the scanner into the generated module.
func TestScanHandlerSites_PackageDirectives(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `// Package billing handles invoicing.
//
//nexus:module billing
//nexus:path /billing
package billing

//nexus:rest GET /invoices
func NewList() {}
`)
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := string(results[0].Content)
	for _, want := range []string{`nexus.Module("billing",`, `nexus.Path("/billing"),`} {
		if !strings.Contains(got, want) {
			t.Errorf("generated module missing %q:\n%s", want, got)
		}
	}

	// A custom decorator on the package doc is refused with a position.
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "h.go"), `// Package h.
//
//nexus:inertia.Page GET /x X
package h

import _ "github.com/paulmanoni/nexus/v2/extension/inertia"

//nexus:query
func NewX() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "function-level") || !strings.Contains(err.Error(), "h.go:3:") {
		t.Fatalf("custom decorator on package doc should be a positioned error, got: %v", err)
	}
}

// TestScanHandlerSites_Routers: //nexus:router on a package doc plus //nexus:on on a
// handler flow end to end into RouterDecl + OnRouter emission.
func TestScanHandlerSites_Routers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `// Package api.
//
//nexus:router v1 /api/v1
//nexus:router billing /billing parent=v1
package api

//nexus:rest GET /invoices
//nexus:on billing
func NewList() {}

//nexus:query
func NewStats() {}
`)
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := string(results[0].Content)
	for _, want := range []string{
		`nexus.RouterDecl("v1", "/api/v1", "")`,
		`nexus.RouterDecl("billing", "/billing", "v1")`,
		`nexus.OnRouter("billing", nexus.AsRest("GET", "/invoices", NewList))`,
		`nexus.AsQuery(NewStats)`, // un-routed op stays on the package module
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated file missing %q:\n%s", want, got)
		}
	}
}

// TestScanHandlerSites_Controller: //nexus:controller on a type and annotations on
// its methods become one nexus.Controller chain; a method of an unannotated
// type registers as a method expression; the type's //nexus:auth is shared.
func TestScanHandlerSites_Controller(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "users.go"), `//nexus:path /admin
package users

import "context"

// UsersController serves the users pages.
//
//nexus:controller /users
//nexus:auth Required
type UsersController struct{}

//nexus:page GET /
func (c *UsersController) Index(ctx context.Context) (string, error) { return "", nil }

//nexus:page GET /:id/view Admin/UserDetail
//nexus:auth Requires view_user
func (c *UsersController) Show(ctx context.Context, id int64) (string, error) { return "", nil }

//nexus:query
func (c UsersController) UserRows(ctx context.Context) ([]string, error) { return nil, nil }

type Health struct{}

//nexus:rest GET /health
func (h *Health) Ping(ctx context.Context) (string, error) { return "", nil }
`)
	writeFile(t, filepath.Join(dir, "more.go"), `package users

import "context"

type Other struct{}

// Index on another type must not collide with UsersController.Index.
//
//nexus:rest GET /other
func (o Other) Index(ctx context.Context) (string, error) { return "", nil }
`)
	results, err := scanHandlerSites(dir, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want one generated file, got %d", len(results))
	}
	got := string(results[0].Content)
	for _, want := range []string{
		`nexus.Path("/admin")`,
		`nexus.Controller[*UsersController]("/users", auth.Required()).`,
		`Rest("GET", "", (*UsersController).Index, inertia.Component("Users/Index")).`,
		`Rest("GET", "/:id/view", (*UsersController).Show, inertia.Component("Admin/UserDetail"), auth.Requires("view_user")).`,
		`Query((*UsersController).UserRows)`,
		"nexus.ControllerActions(func(c *nexus.ControllerRouter[*Health]) {\n\t\t\tc.Rest(\"GET\", \"/health\", (*Health).Ping)\n\t\t}),",
		`nexus.AsRest("GET", "/other", Other.Index)`,
		`"github.com/paulmanoni/nexus/v2/extension/inertia"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated file lacks %s:\n%s", want, got)
		}
	}

	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "x.go"), `package x

//nexus:controller /x
//nexus:rest GET /x
type X struct{}

//nexus:page GET /
func (x *X) Index() (string, error) { return "", nil }
`)
	_, err = scanHandlerSites(bad, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "x.go:4:") || !strings.Contains(err.Error(), "cannot annotate a type") {
		t.Fatalf("a primary on a type should be a positioned error, got: %v", err)
	}
}

// The main module's own package must win the selector over a dependency
// package with the same name (three packages named "utils" is normal in a
// real build graph); only a tie inside the module stays ambiguous.
func TestResolveLayer3PrefersModuleLocal(t *testing.T) {
	dir := t.TempDir()
	dep := filepath.Join(dir, "dep")
	writeFile(t, filepath.Join(dep, "go.mod"), "module example.com/dep\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dep, "foo", "foo.go"), "package foo\n")

	m := filepath.Join(dir, "m")
	writeFile(t, filepath.Join(m, "go.mod"),
		"module example.com/m\n\ngo 1.21\n\nrequire example.com/dep v0.0.0\n\nreplace example.com/dep => ../dep\n")
	writeFile(t, filepath.Join(m, "foo", "foo.go"), "package foo\n")
	writeFile(t, filepath.Join(m, "other", "other.go"), "package other\n\nimport _ \"example.com/dep/foo\"\n")
	annotated := filepath.Join(m, "pages", "p.go")
	writeFile(t, annotated, "package pages\n")

	r := newSelectorResolver(m)
	got, err := r.resolve(annotated, "foo")
	if err != nil {
		t.Fatalf("module-local tie-break: %v", err)
	}
	if got != `"example.com/m/foo"` {
		t.Errorf("module-local tie-break: got %q, want the main module's package", got)
	}
}

// A //nexus:use expression's package selectors resolve through the full cascade —
// a project package needs no import anywhere in the annotated package — while
// an unresolvable identifier (a package-level value, not a package) is skipped
// rather than failing the scan.
func TestResolveUseImportsCascade(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	writeFile(t, filepath.Join(dir, "utils", "u.go"), "package utils\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n\nvar cfg = struct{ Limit int }{}\n")

	r := newSelectorResolver(dir)
	imps, err := resolveUseImports(r, annotated, []string{"wrap(utils.Envelope,", "cfg.Limit)"})
	if err != nil {
		t.Fatal(err)
	}
	if len(imps) != 1 || imps[0] != `"example.com/m/utils"` {
		t.Errorf("use cascade: got %v, want the project's utils import alone", imps)
	}
}

// A //nexus:use identifier that names a package-level declaration of the annotated
// package is a value, not a package: it must be skipped WITHOUT consulting the
// module graph (a miss there forces a `go list -deps` rebuild on every scan —
// ~1s per save on a real app) and without synthesizing a shadowing import.
func TestResolveUseImportsSkipsPackageDecls(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/m\n\ngo 1.21\n")
	annotated := filepath.Join(dir, "pages", "p.go")
	writeFile(t, annotated, "package pages\n\nvar cfg = struct{ Limit int }{}\n\nvar log = struct{ Printf func(string) }{}\n")

	r := newSelectorResolver(dir)
	before := pkgs.listRuns
	imps, err := resolveUseImports(r, annotated, []string{"wrap(cfg.Limit,", "log.Printf)"})
	if err != nil {
		t.Fatal(err)
	}
	if len(imps) != 0 {
		t.Errorf("declared identifiers produced imports %v — a synthesized \"log\" import would collide with the package-level log", imps)
	}
	if got := pkgs.listRuns - before; got != 0 {
		t.Errorf("declared identifiers hit the module graph (%d go list run(s)) — every scan would pay a graph rebuild", got)
	}
}
