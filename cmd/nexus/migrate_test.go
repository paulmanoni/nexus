package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateImportPath(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/paulmanoni/nexus":                            "github.com/paulmanoni/nexus/v2",
		"github.com/paulmanoni/nexus/extension/auth":             "github.com/paulmanoni/nexus/v2/extension/auth",
		"github.com/paulmanoni/nexus/view":                       "github.com/paulmanoni/nexus/v2/view",
		"github.com/paulmanoni/nexus/view/viewgen":               "github.com/paulmanoni/nexus/v2/view/viewgen",
		"github.com/paulmanoni/nexus/httpx/ginrouter":            "github.com/paulmanoni/nexus/httpx/ginrouter/v2",
		"github.com/paulmanoni/nexus/httpx":                      "github.com/paulmanoni/nexus/v2/httpx",
		"github.com/paulmanoni/nexus/extension/cache/redis":      "github.com/paulmanoni/nexus/extension/cache/redis/v2",
		"github.com/paulmanoni/nexus/extension/cache":            "github.com/paulmanoni/nexus/v2/extension/cache",
		"github.com/paulmanoni/nexus/extension/jobs/jobsredis":   "github.com/paulmanoni/nexus/extension/jobs/jobsredis/v2",
		"github.com/paulmanoni/nexus/extension/jobs/jobsamqp":    "github.com/paulmanoni/nexus/extension/jobs/jobsamqp/v2",
		"github.com/paulmanoni/nexus/di/fxcontainer":             "github.com/paulmanoni/nexus/di/fxcontainer/v2",
		"github.com/paulmanoni/nexus/cmd/nexus":                  "github.com/paulmanoni/nexus/cmd/nexus/v2",
		"github.com/paulmanoni/nexus/cmd/nexus/internal/x":       "github.com/paulmanoni/nexus/cmd/nexus/v2/internal/x",
		"github.com/paulmanoni/nexus/v2":                         "",
		"github.com/paulmanoni/nexus/v2/view":                    "",
		"github.com/paulmanoni/nexus/httpx/ginrouter/v2":         "",
		"github.com/paulmanoni/nexus-cloud/sdk":                  "",
		"github.com/paulmanoni/deco/transpiler":                  "",
		"github.com/paulmanoni/nexus/extension/cache/redisextra": "github.com/paulmanoni/nexus/v2/extension/cache/redisextra",
	} {
		got, ok := migrateImportPath(in)
		if want == "" {
			if ok {
				t.Errorf("%s: rewritten to %s, want unchanged", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s → %q (ok=%v), want %q", in, got, ok, want)
		}
	}
}

// migrateFixture is a small v1 project: a handler package with annotations
// in every placement, a templ view, a go.mod and files no rule touches.
var migrateFixture = map[string]string{
	"go.mod": `module example.com/shop

go 1.26

require (
	github.com/a-h/templ v0.3.1020
	github.com/paulmanoni/nexus v1.78.2 // pinned for the release
	github.com/paulmanoni/nexus/httpx/ginrouter v1.78.2
	github.com/paulmanoni/nexus/view v1.78.2
)

require github.com/paulmanoni/nexus/extension/cache/redis v1.78.2 // indirect

replace github.com/paulmanoni/nexus v1.78.2 => ../nexus
`,
	"main.go": `package main

import (
	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/httpx/ginrouter"

	_ "github.com/paulmanoni/nexus/extension/cache/redis"
)

func main() {
	// The string below is data, not an import or an annotation.
	_ = "github.com/paulmanoni/nexus //@rest GET /x"
	nexus.Boot(nexus.WithRouter(ginrouter.New()))
}
`,
	"orders/orders.go": `// Package orders serves orders.
//
//@module shop
// @path /shop
package orders

import (
	"context"

	nx "github.com/paulmanoni/nexus"
	"github.com/paulmanoni/nexus/extension/auth"
)

var _ = auth.Required

//@controller /orders
//@auth Required
type OrdersController struct{ app *nx.App }

//@provide
func NewOrdersController(app *nx.App) *OrdersController { return &OrdersController{app} }

//@rest GET /:id
// @auth Requires view_orders
// Show returns one order.
//
// @Summary Show an order (another tool's annotation, left alone)
func (c *OrdersController) Show(ctx context.Context, id int64) (string, error) { return "", nil }

// @inertia.Page GET /orders Orders/Index
//@contact.name not nexus either
func NewIndex() {}
`,
	"views/home.templ": `package views

import (
	"strings"

	"github.com/paulmanoni/nexus/view"
)

//@page GET /
// @auth Required
templ Home() {
	@Counter()
	<p>{ strings.ToUpper("@rest") }</p>
}

templ Counter() {
	{{ c := view.State(ctx, 0) }}
	<button>{ c.Get() }</button>
}
`,
	"views/single.templ": `package views

import "github.com/paulmanoni/nexus/view"

templ Single() {
	{{ _ = view.State(ctx, 0) }}
}
`,
	"README.md":                "//@rest GET /x stays in docs\n",
	"node_modules/x/index.go":  "package x\n\nimport _ \"github.com/paulmanoni/nexus\"\n",
	"testdata/fixture/main.go": "package main\n\nimport _ \"github.com/paulmanoni/nexus\"\n",
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(rel)), body)
	}
	return root
}

func readTree(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runMigrateV2(t *testing.T, args ...string) string {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := newRootCmd(&out, &errb)
	cmd.SetArgs(append([]string{"migrate", "v2"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate v2 %v: %v\n%s", args, err, errb.String())
	}
	return out.String()
}

func TestMigrateV2_Project(t *testing.T) {
	root := writeTree(t, migrateFixture)
	report := runMigrateV2(t, root)

	for _, want := range []string{
		"go.mod  (go.mod 5)",
		"main.go  (imports 3)",
		"orders/orders.go  (imports 2, annotations 8)",
		"views/home.templ  (imports 1, annotations 2)",
		"views/single.templ  (imports 1)",
		"migrated 5 file(s)",
		"go mod tidy",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}

	gomod := readTree(t, root, "go.mod")
	for _, want := range []string{
		"\tgithub.com/paulmanoni/nexus/v2 v2.0.0 // pinned for the release\n",
		"\tgithub.com/paulmanoni/nexus/httpx/ginrouter/v2 v2.0.0\n",
		"require github.com/paulmanoni/nexus/extension/cache/redis/v2 v2.0.0 // indirect\n",
		"replace github.com/paulmanoni/nexus/v2 => ../nexus\n",
		"github.com/a-h/templ v0.3.1020",
	} {
		if !strings.Contains(gomod, want) {
			t.Errorf("go.mod missing %q:\n%s", want, gomod)
		}
	}
	if strings.Contains(gomod, "nexus/view") || strings.Contains(gomod, "v1.78.2") {
		t.Errorf("go.mod keeps a v1 requirement:\n%s", gomod)
	}

	main := readTree(t, root, "main.go")
	for _, want := range []string{
		`"github.com/paulmanoni/nexus/v2"`,
		`"github.com/paulmanoni/nexus/httpx/ginrouter/v2"`,
		`_ "github.com/paulmanoni/nexus/extension/cache/redis/v2"`,
		`_ = "github.com/paulmanoni/nexus //@rest GET /x"`, // string literal untouched
	} {
		if !strings.Contains(main, want) {
			t.Errorf("main.go missing %q:\n%s", want, main)
		}
	}

	orders := readTree(t, root, "orders/orders.go")
	for _, want := range []string{
		// Package doc: gofmt keeps the directives below the prose.
		"// Package orders serves orders.\n//\n//nexus:module shop\n//nexus:path /shop\npackage orders",
		`nx "github.com/paulmanoni/nexus/v2"`,
		`"github.com/paulmanoni/nexus/v2/extension/auth"`,
		"//nexus:controller /orders\n//nexus:auth Required\ntype OrdersController",
		"//nexus:provide\nfunc NewOrdersController",
		// gofmt moves the directives after the prose, in their order.
		"// Show returns one order.\n//\n// @Summary Show an order (another tool's annotation, left alone)\n//\n//nexus:rest GET /:id\n//nexus:auth Requires view_orders\nfunc (c *OrdersController) Show",
		"//nexus:inertia.Page GET /orders Orders/Index\n",
		"// @contact.name not nexus either", // lowercase-qualified: another tool's (gofmt spaces it, as in v1)
	} {
		if !strings.Contains(orders, want) {
			t.Errorf("orders.go missing %q:\n%s", want, orders)
		}
	}

	home := readTree(t, root, "views/home.templ")
	for _, want := range []string{
		`"github.com/paulmanoni/nexus/v2/view"`,
		"//nexus:page GET /\n//nexus:auth Required\ntempl Home()",
		`strings.ToUpper("@rest")`,
		"\t@Counter()\n",
	} {
		if !strings.Contains(home, want) {
			t.Errorf("home.templ missing %q:\n%s", want, home)
		}
	}
	if single := readTree(t, root, "views/single.templ"); !strings.Contains(single, `import "github.com/paulmanoni/nexus/v2/view"`) {
		t.Errorf("single-line templ import not rewritten:\n%s", single)
	}

	// Skipped trees and non-source files are untouched.
	for rel, body := range map[string]string{
		"README.md":                migrateFixture["README.md"],
		"node_modules/x/index.go":  migrateFixture["node_modules/x/index.go"],
		"testdata/fixture/main.go": migrateFixture["testdata/fixture/main.go"],
	} {
		if got := readTree(t, root, rel); got != body {
			t.Errorf("%s changed:\n%s", rel, got)
		}
	}

	// Idempotent: a migrated tree migrates to itself.
	if again := runMigrateV2(t, root); !strings.Contains(again, "nothing to migrate") {
		t.Errorf("second run changed files:\n%s", again)
	}
}

// --dry-run reports every edit and writes nothing.
func TestMigrateV2_DryRun(t *testing.T) {
	root := writeTree(t, migrateFixture)
	report := runMigrateV2(t, "--dry-run", root)
	for _, want := range []string{
		`imports:4  "github.com/paulmanoni/nexus" → "github.com/paulmanoni/nexus/v2"`,
		"go.mod:9  require github.com/paulmanoni/nexus/view v1.78.2 → (dropped)",
		"annotations:3  //@module shop → //nexus:module shop",
		"annotations:4  // @path /shop → //nexus:path /shop",
		"dry run: would migrate 5 file(s)",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("dry-run report missing %q:\n%s", want, report)
		}
	}
	for rel, body := range migrateFixture {
		if got := readTree(t, root, rel); got != body {
			t.Errorf("dry run wrote %s", rel)
		}
	}
}

// A go.mod that required only the view module still gets the root, and a
// half-migrated one (old and new path both required) keeps one requirement.
func TestMigrateV2_GoModEdges(t *testing.T) {
	cases := map[string]struct{ in, want, absent string }{
		"view-only": {
			in:     "module example.com/pets\n\ngo 1.26\n\nrequire github.com/paulmanoni/nexus/view v1.78.2\n",
			want:   "github.com/paulmanoni/nexus/v2 v2.0.0",
			absent: "nexus/view",
		},
		"half-migrated": {
			in:     "module example.com/pets\n\ngo 1.26\n\nrequire (\n\tgithub.com/paulmanoni/nexus v1.78.2\n\tgithub.com/paulmanoni/nexus/v2 v2.0.0\n)\n",
			want:   "require github.com/paulmanoni/nexus/v2 v2.0.0\n",
			absent: "v1.78.2",
		},
		"view-replace": {
			in:     "module example.com/pets\n\ngo 1.26\n\nrequire github.com/paulmanoni/nexus v1.78.2\n\nreplace github.com/paulmanoni/nexus/view => ../nexus/view\n",
			want:   "require github.com/paulmanoni/nexus/v2 v2.0.0",
			absent: "replace",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, changes, err := migrateGoMod("go.mod", []byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if len(changes) == 0 || !strings.Contains(string(out), tc.want) || strings.Contains(string(out), tc.absent) {
				t.Fatalf("got:\n%s", out)
			}
			if again, more, _ := migrateGoMod("go.mod", out); len(more) != 0 || !bytes.Equal(again, out) {
				t.Fatalf("not idempotent: %v\n%s", more, again)
			}
		})
	}
}

// legacyAnnotation recognizes nexus annotations only.
func TestLegacyAnnotation(t *testing.T) {
	for text, want := range map[string]string{
		"//@rest GET /x":           "rest",
		"// @query":                "query",
		"//\t@auth Required":       "auth",
		"//@inertia.Page GET / A":  "inertia.Page",
		"//@widgets.Panel /stats":  "widgets.Panel",
		"// @Summary List":         "",
		"// @Router /x [get]":      "",
		"//@contact.name Support":  "",
		"//@securityDefinitions.x": "",
		"//nexus:rest GET /x":      "",
		"// @every 5m":             "",
		"/* @rest */":              "",
		"// email me @rest":        "",
	} {
		kw, _, ok := legacyAnnotation(text)
		if (want == "") == ok || kw != want {
			t.Errorf("%q → %q (ok=%v), want %q", text, kw, ok, want)
		}
	}
}
