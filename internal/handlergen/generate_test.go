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
