package viewgen

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/a-h/templ/parser/v2"
)

// The package pass registers a package's pages and shards so the app needs
// no wiring:
//
//	//@page GET /
//	templ Home() { … }
//
// A component whose server code reads a signal is a shard. Its endpoint
// takes its gates from its own //@auth / //@use lines, or else inherits
// them from the pages that render it — which must all agree — so a shard is
// never reachable with fewer gates than its page. Every view.Use[T] in a
// template gets its view.Expose[T]().

const (
	nexusImport = "github.com/paulmanoni/nexus"
	authImport  = "github.com/paulmanoni/nexus/extension/auth"
)

// directives reads the directive lines of a component's doc comment: Go's
// directive form //nexus:page GET / (the nexus 2.0 spelling) or the v1
// //@page GET /. Both register the same thing.
func (f *fileRewriter) directives(info *Component, doc string, at parser.Position) bool {
	ok := true
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		rest, found := strings.CutPrefix(line, "//nexus:")
		if !found {
			if !strings.HasPrefix(line, "//") {
				continue
			}
			if rest, found = strings.CutPrefix(strings.TrimSpace(strings.TrimPrefix(line, "//")), "@"); !found {
				continue
			}
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		switch kw, args := fields[0], fields[1:]; kw {
		case "page":
			if len(args) != 2 || !strings.HasPrefix(args[1], "/") {
				f.fail(at, "//@page takes a method and a path: //@page GET /pets")
				ok = false
				continue
			}
			info.Method, info.Path = strings.ToUpper(args[0]), args[1]
		case "auth":
			gate, err := authGate(args)
			if err != nil {
				f.fail(at, "%v", err)
				ok = false
				continue
			}
			info.Gates, info.HasGates = append(info.Gates, gate), true
		case "use":
			expr := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "use"))
			if _, err := goparser.ParseExpr(expr); err != nil {
				f.fail(at, "//@use takes a Go expression: %v", err)
				ok = false
				continue
			}
			info.Gates, info.HasGates = append(info.Gates, expr), true
		default:
			f.fail(at, "unknown directive //@%s — a component takes //@page, //@auth and //@use", kw)
			ok = false
		}
	}
	if info.Method != "" && info.Params > 0 {
		f.fail(at, "%s is a //@page, so it takes no parameters", info.Name)
		ok = false
	}
	return ok
}

// authGate turns //@auth arguments into the gate option, with nexus
// decorator grammar: Required, Public, Requires PERM…, or bare permissions.
func authGate(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("//@auth takes Required, Public, or permissions")
	}
	switch args[0] {
	case "Required":
		return "auth.Required()", nil
	case "Public":
		return "nexus.Public()", nil
	case "Requires":
		args = args[1:]
		if len(args) == 0 {
			return "", errors.New("//@auth Requires names at least one permission")
		}
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strconv.Quote(a)
	}
	return "auth.Requires(" + strings.Join(quoted, ", ") + ")", nil
}

var useType = regexp.MustCompile(`view\.Use\[([^\]]+)\]`)

// usedTypes lists the types a component reads with view.Use.
func usedTypes(t *parser.HTMLTemplate) []string {
	var out []string
	var walk func(ns []parser.Node)
	scan := func(s string) {
		for _, m := range useType.FindAllStringSubmatch(s, -1) {
			out = append(out, strings.TrimSpace(m[1]))
		}
	}
	walk = func(ns []parser.Node) {
		for _, n := range ns {
			switch n := n.(type) {
			case *parser.GoCode:
				scan(n.Expression.Value)
			case *parser.StringExpression:
				scan(n.Expression.Value)
			case *parser.TemplElementExpression:
				scan(n.Expression.Value)
			case *parser.IfExpression:
				scan(n.Expression.Value)
			case *parser.ForExpression:
				scan(n.Expression.Value)
			case *parser.SwitchExpression:
				scan(n.Expression.Value)
			case *parser.Element:
				for _, a := range n.Attributes {
					if ea, ok := a.(*parser.ExpressionAttribute); ok {
						scan(ea.Expression.Value)
					}
				}
			}
			if cn, ok := n.(parser.CompositeNode); ok {
				walk(cn.ChildNodes())
			}
		}
	}
	walk(t.Children)
	return out
}

// Scan reads what the Go files in dir declare: shards and pages registered
// by hand, state structs (types with *view.Signal fields) and view.Expose
// calls.
func Scan(dir string) (*Package, error) {
	pkg := &Package{Shards: map[string]bool{}, Pages: map[string]bool{}, States: map[string][]string{}, Exposed: map[string]bool{}}
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_templ.go") || strings.HasSuffix(path, "view_gen.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		file, err := goparser.ParseFile(token.NewFileSet(), path, src, 0)
		if err != nil {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeSpec:
				st, ok := n.Type.(*ast.StructType)
				if !ok {
					return true
				}
				for _, f := range st.Fields.List {
					if !strings.Contains(string(src[f.Type.Pos()-1:f.Type.End()-1]), "view.Signal") {
						continue
					}
					for _, name := range f.Names {
						if name.IsExported() {
							pkg.States[n.Name.Name] = append(pkg.States[n.Name.Name], name.Name)
						}
					}
				}
			case *ast.CallExpr:
				switch {
				case isViewCall(n, "Shard") && len(n.Args) > 0:
					if id, ok := n.Args[0].(*ast.Ident); ok {
						pkg.Shards[id.Name] = true
					}
				case isViewCall(n, "Page") && len(n.Args) > 2:
					if id, ok := n.Args[2].(*ast.Ident); ok {
						pkg.Pages[id.Name] = true
					}
				case isViewCall(n, "Expose"):
					if ix, ok := n.Fun.(*ast.IndexExpr); ok {
						pkg.Exposed[string(src[ix.Index.Pos()-1:ix.Index.End()-1])] = true
					}
				}
			}
			return true
		})
	}
	return pkg, nil
}

// isViewCall reports whether e is a call view.name(…) or view.name[T](…).
func isViewCall(e ast.Expr, name string) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	fun := call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == name && isIdent(sel.X, "view")
}

// Registrations is the package's view_gen.go: its compiled expressions, and
// a deferred nexus option registering its pages, shards and exposed types.
// all holds every component of the module by ID — the render graph a
// shard's gates are inherited through; nil means this package alone. It is
// nil when there is nothing to register.
func Registrations(results []*Result, pkg *Package, all map[string]*Component) ([]byte, error) {
	if pkg == nil {
		pkg = &Package{}
	}
	if all == nil {
		all = map[string]*Component{}
		for _, r := range results {
			for _, c := range r.Components {
				all[c.ID] = c
			}
		}
	}
	twins := map[string]string{}
	comps := map[string]*Component{}
	imports := map[string]string{}
	pkgName := ""
	for _, r := range results {
		pkgName = r.Package
		for id, js := range r.Twins {
			twins[id] = js
		}
		for _, c := range r.Components {
			comps[c.Name] = c
		}
		for sel, line := range r.Imports {
			imports[sel] = line
		}
	}
	names := make([]string, 0, len(comps))
	for n := range comps {
		names = append(names, n)
	}
	sort.Strings(names)

	var opts []string
	var errList []error
	for _, n := range names {
		c := comps[n]
		if c.Method != "" && !pkg.Pages[n] {
			opts = append(opts, fmt.Sprintf("view.Page(%q, %q, %s%s)", c.Method, c.Path, n, optList(c.Gates)))
		}
	}
	for _, n := range names {
		c := comps[n]
		if !c.Shard || pkg.Shards[n] {
			if c.HasGates && c.Method == "" && !c.Shard {
				errList = append(errList, c.errorf("//@auth and //@use gate pages and shards, and %s is neither", n))
			}
			continue
		}
		gates, from, err := shardGates(c, all)
		if err != nil {
			errList = append(errList, err)
			continue
		}
		for sel, line := range from {
			if _, ok := imports[sel]; !ok {
				imports[sel] = line
			}
		}
		opts = append(opts, fmt.Sprintf("view.Shard(%s%s)", n, optList(gates)))
	}
	exposed := map[string]bool{}
	for _, n := range names {
		for _, t := range comps[n].Uses {
			if !exposed[t] && !pkg.Exposed[t] {
				exposed[t] = true
				opts = append(opts, fmt.Sprintf("view.Expose[%s]()", t))
			}
		}
	}
	if len(errList) > 0 {
		return nil, errors.Join(errList...)
	}
	if len(twins) == 0 && len(opts) == 0 {
		return nil, nil
	}

	lines := map[string]bool{strconv.Quote(ViewImport): true}
	if len(opts) > 0 {
		lines[strconv.Quote(nexusImport)] = true
	}
	for _, o := range opts {
		e, _ := goparser.ParseExpr(o)
		ast.Inspect(e, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					switch {
					case id.Name == "auth":
						lines[strconv.Quote(authImport)] = true
					case id.Name != "view" && id.Name != "nexus" && imports[id.Name] != "":
						lines[imports[id.Name]] = true
					}
				}
			}
			return true
		})
	}
	sortedImports := make([]string, 0, len(lines))
	for l := range lines {
		sortedImports = append(sortedImports, l)
	}
	sort.Strings(sortedImports)

	ids := make([]string, 0, len(twins))
	for id := range twins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by nexus; DO NOT EDIT.\n\npackage %s\n\nimport (\n", pkgName)
	for _, l := range sortedImports {
		fmt.Fprintf(&b, "\t%s\n", l)
	}
	b.WriteString(")\n\nfunc init() {\n")
	if len(ids) > 0 {
		b.WriteString("\tview.RegisterTwins(map[string]string{\n")
		for _, id := range ids {
			fmt.Fprintf(&b, "\t\t%q: %q,\n", id, twins[id])
		}
		b.WriteString("\t})\n")
	}
	if len(opts) > 0 {
		b.WriteString("\tnexus.RegisterDeferredOptions(func() []nexus.Option {\n\t\treturn []nexus.Option{\n")
		for _, o := range opts {
			fmt.Fprintf(&b, "\t\t\t%s,\n", o)
		}
		b.WriteString("\t\t}\n\t})\n")
	}
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

func optList(gates []string) string {
	if len(gates) == 0 {
		return ""
	}
	return ", " + strings.Join(gates, ", ")
}

func (c *Component) errorf(format string, args ...any) error {
	return &PositionError{File: c.At.File, Line: c.At.Line, Col: c.At.Col, Msg: fmt.Sprintf(format, args...)}
}

// shardGates chooses a shard's gates: its own, or those of every page that
// renders it — anywhere in the module — which must agree.
func shardGates(c *Component, all map[string]*Component) ([]string, map[string]string, error) {
	if c.HasGates {
		return c.Gates, nil, nil
	}
	var pages []*Component
	for _, p := range all {
		if p.Method != "" && reaches(p, c.ID, all, map[string]bool{}) {
			pages = append(pages, p)
		}
	}
	sort.Slice(pages, func(i, j int) bool { return pages[i].ID < pages[j].ID })
	if len(pages) == 0 {
		return nil, nil, c.errorf("%s is a shard (it reads a signal on the server), and no //@page renders it — give it its own gates: //@auth Required, or //@auth Public", c.Name)
	}
	gates := pages[0].Gates
	for _, p := range pages[1:] {
		if strings.Join(p.Gates, "\x00") != strings.Join(gates, "\x00") {
			names := make([]string, len(pages))
			for i, p := range pages {
				names[i] = shortID(p.ID)
			}
			return nil, nil, c.errorf("%s is a shard rendered by pages with different gates (%s) — give it its own //@auth", c.Name, strings.Join(names, ", "))
		}
	}
	return gates, pages[0].Imports, nil
}

// shortID is "pkg.Name" for an ID "example.com/app/pkg.Name".
func shortID(id string) string { return id[strings.LastIndexByte(id, '/')+1:] }

func reaches(from *Component, target string, all map[string]*Component, seen map[string]bool) bool {
	if seen[from.ID] {
		return false
	}
	seen[from.ID] = true
	for _, call := range from.Calls {
		if call == target {
			return true
		}
		if next, ok := all[call]; ok && reaches(next, target, all, seen) {
			return true
		}
	}
	return false
}

// writeIfChanged writes content to path unless it is already there, and
// reports whether it wrote.
func writeIfChanged(path string, content []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	return true, os.WriteFile(path, content, 0o644)
}
