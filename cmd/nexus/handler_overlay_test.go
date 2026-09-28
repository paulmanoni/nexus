package main

import (
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
// a lowercase //@rest method is normalised to the HTTP verb, a typo'd keyword
// is a positioned error with a suggestion, a malformed directive reports the
// annotation's own file:line, and genuinely foreign keywords stay ignored.
func TestScanHandlerSites_Strictness(t *testing.T) {
	// Lowercase method normalises; the route registers as GET.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `package h

//@rest get /users
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

//@quer
func NewList() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "did you mean //@query") {
		t.Fatalf("typo should suggest //@query, got: %v", err)
	}
	if !strings.Contains(err.Error(), "h.go:3:") {
		t.Fatalf("typo error should carry file:line, got: %v", err)
	}

	// A malformed known directive reports the annotation's file:line.
	dir3 := t.TempDir()
	writeFile(t, filepath.Join(dir3, "h.go"), `package h

//@rest GET users
func NewList() {}
`)
	_, err = scanHandlerSites(dir3, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "h.go:3:") || !strings.Contains(err.Error(), `must start with "/"`) {
		t.Fatalf("malformed //@rest should carry file:line and the rule, got: %v", err)
	}

	// A genuinely foreign keyword stays ignored (coexistence with other tools).
	dir4 := t.TempDir()
	writeFile(t, filepath.Join(dir4, "h.go"), `package h

//@deprecated
//@query
func NewList() {}
`)
	if _, err := scanHandlerSites(dir4, "nexus_handlers_gen.go"); err != nil {
		t.Fatalf("foreign keyword must not error: %v", err)
	}
}

// TestScanHandlerSites_InertiaPage: the //@inertia.Page decorator end to end —
// bare tokens resolve, normalise, and emit a quoted registrar call; a bad verb
// is a positioned error at the annotation.
func TestScanHandlerSites_InertiaPage(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `package h

import _ "github.com/paulmanoni/nexus/extension/inertia"

//@inertia.Page get,post /login Login
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

import _ "github.com/paulmanoni/nexus/extension/inertia"

//@inertia.Page FETCH /login Login
func NewLogin() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "not an HTTP method") || !strings.Contains(err.Error(), "h.go:5:") {
		t.Fatalf("bad verb should be a positioned annotation error, got: %v", err)
	}
}

// TestScanHandlerSites_PackageDirectives: //@module and //@path on the
// package doc comment flow through the scanner into the generated module.
func TestScanHandlerSites_PackageDirectives(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `// Package billing handles invoicing.
//
//@module billing
//@path /billing
package billing

//@rest GET /invoices
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
//@inertia.Page GET /x X
package h

import _ "github.com/paulmanoni/nexus/extension/inertia"

//@query
func NewX() {}
`)
	_, err = scanHandlerSites(dir2, "nexus_handlers_gen.go")
	if err == nil || !strings.Contains(err.Error(), "function-level") || !strings.Contains(err.Error(), "h.go:3:") {
		t.Fatalf("custom decorator on package doc should be a positioned error, got: %v", err)
	}
}

// TestScanHandlerSites_Routers: //@router on a package doc plus //@on on a
// handler flow end to end into RouterDecl + OnRouter emission.
func TestScanHandlerSites_Routers(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "h.go"), `// Package api.
//
//@router v1 /api/v1
//@router billing /billing parent=v1
package api

//@rest GET /invoices
//@on billing
func NewList() {}

//@query
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
