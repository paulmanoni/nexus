package main

import (
	"flag"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateMigrateGolden = flag.Bool("update-migrate", false, "rewrite testdata/migrate/*/*.golden")

// TestMigrateV2_SymbolFixtures runs the whole v2 rule table over each
// testdata/migrate/symbols/*.input file and compares the result with its
// .golden. Every output must parse, and migrating it again changes nothing.
func TestMigrateV2_SymbolFixtures(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "migrate", "symbols", "*.input"))
	if err != nil || len(inputs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var goRules []migrateRule
	for _, r := range migrateV2Rules {
		if r.Applies("x.go") {
			goRules = append(goRules, r)
		}
	}
	for _, in := range inputs {
		name := strings.TrimSuffix(filepath.Base(in), ".input")
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(in)
			if err != nil {
				t.Fatal(err)
			}
			res, err := migrateFile(name+".go", src, goRules)
			if err != nil {
				t.Fatal(err)
			}
			if res == nil {
				t.Fatal("nothing migrated")
			}
			golden := strings.TrimSuffix(in, ".input") + ".golden"
			if *updateMigrateGolden {
				if err := os.WriteFile(golden, res.Out, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update-migrate to create it)", err)
			}
			if string(res.Out) != string(want) {
				t.Errorf("output differs from %s:\n%s", golden, res.Out)
			}
			if _, err := parser.ParseFile(token.NewFileSet(), "", res.Out, parser.AllErrors); err != nil {
				t.Errorf("output does not parse: %v", err)
			}
			again, err := migrateFile(name+".go", res.Out, goRules)
			if err != nil {
				t.Fatal(err)
			}
			if again != nil {
				t.Errorf("second run changed %v:\n%s", again.Rules, again.Out)
			}
		})
	}
}

// Only selectors on the nexus import move; the same names on another
// package, a local variable or a field stay.
func TestRewriteGoSymbols_OnlyNexusSelectors(t *testing.T) {
	src := `package p

import (
	"os"

	"github.com/paulmanoni/nexus/v2"
)

type T struct{ Config int }

func f(t T) {
	_ = os.Getenv("x")
	_ = t.Config
	_ = nexus.Module("m")
}
`
	out, changes, err := migrateGoSymbols("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || string(out) != src {
		t.Fatalf("changed %v:\n%s", changes, out)
	}
}

// A row is all a later move needs: the table drives matching, the target
// import and the report.
func TestRewriteGoSymbols_TableDriven(t *testing.T) {
	table := []migrateSymbol{{From: "", Old: "PreserveDev", To: "dev", New: "Preserve"}}
	src := `package p

import "github.com/paulmanoni/nexus/v2"

func f(s any) { nexus.PreserveDev("store", s) }
`
	out, changes, err := rewriteGoSymbols([]byte(src), table)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if len(changes) != 1 || changes[0].Old != "nexus.PreserveDev" || changes[0].New != "dev.Preserve" {
		t.Errorf("changes = %+v", changes)
	}
	if !strings.Contains(got, `import "github.com/paulmanoni/nexus/v2/dev"`) ||
		strings.Contains(got, `"github.com/paulmanoni/nexus/v2"`+"\n") ||
		!strings.Contains(got, `dev.Preserve("store", s)`) {
		t.Errorf("got:\n%s", got)
	}
}

// The report and --dry-run list the symbol rule's edits.
func TestMigrateV2_SymbolsReport(t *testing.T) {
	root := writeTree(t, map[string]string{
		"main.go": "package main\n\nimport \"github.com/paulmanoni/nexus\"\n\nfunc main() { nexus.Run(nexus.Config{}) }\n",
	})
	report := runMigrateV2(t, "--dry-run", root)
	for _, want := range []string{
		"main.go  (imports 1, symbols 1)",
		"symbols:5  nexus.Config → config.Runtime",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}
