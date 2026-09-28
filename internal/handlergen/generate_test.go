package handlergen

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerate_OneFilePerPackage(t *testing.T) {
	sites := []Site{
		{Dir: "/app/users", Pkg: "users", Func: "NewSvc", Keyword: "provide", Line: 1},
		{Dir: "/app/users", Pkg: "users", Func: "NewGet", Keyword: "rest", Args: []string{"GET", "/u/:id"}, Line: 2},
		{Dir: "/app/billing", Pkg: "billing", Func: "NewCharge", Keyword: "mutation", Line: 1},
	}
	results, err := Generate(sites, "nexus_handlers_gen.go")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(results), results)
	}
	// Sorted by path: billing before users.
	if results[0].Path != filepath.Join("/app/billing", "nexus_handlers_gen.go") {
		t.Fatalf("result[0].Path = %s", results[0].Path)
	}
	if results[1].Path != filepath.Join("/app/users", "nexus_handlers_gen.go") {
		t.Fatalf("result[1].Path = %s", results[1].Path)
	}
	if !strings.Contains(string(results[0].Content), "package billing") ||
		!strings.Contains(string(results[0].Content), `decorate.Register(nexus.Module("billing",`) ||
		!strings.Contains(string(results[0].Content), "nexus.AsMutation(NewCharge)") {
		t.Fatalf("billing content wrong:\n%s", results[0].Content)
	}
	if !strings.Contains(string(results[1].Content), "package users") ||
		!strings.Contains(string(results[1].Content), `decorate.Register(nexus.Module("users",`) ||
		!strings.Contains(string(results[1].Content), `nexus.AsRest("GET", "/u/:id", NewGet)`) {
		t.Fatalf("users content wrong:\n%s", results[1].Content)
	}
}

func TestGenerate_SkipsPackagesWithNoRegistrations(t *testing.T) {
	// All-empty input yields no files.
	results, err := Generate(nil, "gen.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestGenerate_ConflictingPackagesErrors(t *testing.T) {
	sites := []Site{
		{Dir: "/app/x", Pkg: "a", Func: "F", Keyword: "query", Line: 1},
		{Dir: "/app/x", Pkg: "b", Func: "G", Keyword: "query", Line: 2},
	}
	if _, err := Generate(sites, "gen.go"); err == nil {
		t.Fatal("expected conflicting-packages error")
	}
}

func TestGenerate_Deterministic(t *testing.T) {
	sites := []Site{
		{Dir: "/z", Pkg: "z", Func: "NewZ", Keyword: "query", Line: 1},
		{Dir: "/a", Pkg: "a", Func: "NewA", Keyword: "query", Line: 1},
	}
	r1, _ := Generate(sites, "g.go")
	r2, _ := Generate(sites, "g.go")
	if len(r1) != 2 || r1[0].Path != r2[0].Path || string(r1[0].Content) != string(r2[0].Content) {
		t.Fatal("Generate not deterministic / not path-sorted")
	}
	if !strings.HasPrefix(r1[0].Path, "/a") {
		t.Fatalf("results not sorted by path: %s", r1[0].Path)
	}
}

// TestGenerate_PackageDirectives: //@module renames the group, //@path and
// //@routeprefix become the module's leading options, duplicates agreeing
// across files dedupe, conflicts and scope misuse are positioned errors.
func TestGenerate_PackageDirectives(t *testing.T) {
	fn := Site{Dir: "d", Pkg: "billing", File: "d/h.go", Func: "NewCharge",
		Keyword: "rest", Args: []string{"POST", "/charge"}, Line: 10}

	res, err := Generate([]Site{
		{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "module", Args: []string{"billing-api"}, Line: 3, PackageLevel: true},
		{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "path", Args: []string{"/billing"}, Line: 4, PackageLevel: true},
		{Dir: "d", Pkg: "billing", File: "d/other.go", Keyword: "module", Args: []string{"billing-api"}, Line: 2, PackageLevel: true}, // agreeing duplicate
		fn,
	}, "gen.go")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	got := string(res[0].Content)
	for _, want := range []string{
		`nexus.Module("billing-api",`,
		`nexus.Path("/billing"),`,
		`nexus.AsRest("POST", "/charge", NewCharge)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "nexus.Path") > strings.Index(got, "nexus.AsRest") {
		t.Errorf("nexus.Path must lead the option list:\n%s", got)
	}

	cases := []struct {
		name string
		site Site
		want string
	}{
		{"conflict", Site{Dir: "d", Pkg: "billing", File: "d/other.go", Keyword: "module",
			Args: []string{"other"}, Line: 2, PackageLevel: true}, "conflicts with"},
		{"function-level misuse", Site{Dir: "d", Pkg: "billing", File: "d/h.go", Keyword: "path",
			Args: []string{"/x"}, Func: "NewCharge", Line: 9}, "package-level — put it on the package doc comment"},
		{"function keyword on package doc", Site{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "query",
			Line: 5, PackageLevel: true}, "not a package-level directive"},
		{"bad prefix", Site{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "path",
			Args: []string{"billing"}, Line: 4, PackageLevel: true}, `must start with "/"`},
		{"module missing name", Site{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "module",
			Line: 3, PackageLevel: true}, "needs exactly a module name"},
	}
	base := []Site{
		{Dir: "d", Pkg: "billing", File: "d/doc.go", Keyword: "module", Args: []string{"billing-api"}, Line: 3, PackageLevel: true},
		fn,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Generate(append(append([]Site{}, base...), c.site), "gen.go")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want containing %q", err, c.want)
			}
			if !strings.Contains(err.Error(), c.site.File+":") {
				t.Fatalf("error %v lacks position from %s", err, c.site.File)
			}
		})
	}
}

// TestGenerate_Routers: //@router declarations emit RouterDecl calls (in the
// declaring package's file, even with no other registrations there), //@on
// wraps the op in nexus.OnRouter, cross-package references work, and the
// error shapes are positioned.
func TestGenerate_Routers(t *testing.T) {
	decl := func(dir, file string, line int, args ...string) Site {
		return Site{Dir: dir, Pkg: "api", File: file, Keyword: "router", Args: args, Line: line, PackageLevel: true}
	}
	res, err := Generate([]Site{
		decl("api", "api/doc.go", 3, "v1", "/api/v1"),
		decl("api", "api/doc.go", 4, "billing", "/billing", "parent=v1", "auth=Requires(ADMIN,HR)"),
		{Dir: "b", Pkg: "billing", File: "b/h.go", Func: "NewList",
			Keyword: "rest", Args: []string{"GET", "/invoices"}, Line: 10},
		{Dir: "b", Pkg: "billing", File: "b/h.go", Func: "NewList",
			Keyword: "on", Args: []string{"billing"}, Line: 11},
	}, "gen.go")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 files (decl-only package + ops package), got %d", len(res))
	}
	declFile, opsFile := string(res[0].Content), string(res[1].Content)
	for _, want := range []string{
		`nexus.RouterDecl("v1", "/api/v1", "")`,
		`nexus.RouterDecl("billing", "/billing", "v1", auth.Requires("ADMIN", "HR"))`,
		`"github.com/paulmanoni/nexus/extension/auth"`,
	} {
		if !strings.Contains(declFile, want) {
			t.Errorf("decl file missing %q:\n%s", want, declFile)
		}
	}
	if !strings.Contains(opsFile, `nexus.OnRouter("billing", nexus.AsRest("GET", "/invoices", NewList))`) {
		t.Errorf("ops file missing OnRouter wrap:\n%s", opsFile)
	}

	base := []Site{decl("api", "api/doc.go", 3, "v1", "/api/v1")}
	fn := func(kw string, args ...string) Site {
		return Site{Dir: "b", Pkg: "b", File: "b/h.go", Func: "F", Keyword: kw, Args: args, Line: 9}
	}
	cases := []struct {
		name  string
		sites []Site
		want  string
	}{
		{"conflicting decl", append(base, decl("x", "x/doc.go", 2, "v1", "/other")), "conflicts with its declaration at api/doc.go:3"},
		{"unknown parent", []Site{decl("api", "api/doc.go", 3, "a", "/a", "parent=ghost")}, `unknown parent "ghost"`},
		{"cycle", []Site{decl("api", "api/doc.go", 3, "a", "/a", "parent=b"), decl("api", "api/doc.go", 4, "b", "/b", "parent=a")}, "forms a cycle"},
		{"bad prefix", []Site{decl("api", "api/doc.go", 3, "a", "a")}, `must start with "/"`},
		{"router on a function", []Site{{Dir: "b", Pkg: "b", File: "b/h.go", Func: "F", Keyword: "router", Args: []string{"a", "/a"}, Line: 9}}, "package-level"},
		{"unknown on", append(append([]Site{}, base...), fn("rest", "GET", "/x"), fn("on", "biling")), `unknown router "biling"`},
		{"on did-you-mean", append(append([]Site{}, base...), fn("rest", "GET", "/x"), fn("on", "v2")), `did you mean "v1"?`},
		{"bad auth", []Site{decl("api", "api/doc.go", 3, "a", "/a", "auth=Admin")}, "not Required or Requires"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Generate(c.sites, "gen.go")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want containing %q", err, c.want)
			}
		})
	}
}
