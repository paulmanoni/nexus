package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// migrateGoFields renames struct fields v2 renamed, in composite literals of
// the nexus type that owns them — middleware.Middleware{Gin: …} becomes
// middleware.Middleware{HTTP: …}. A field read through a variable
// (mw.Gin) has no type to check here; the build names those.
var migrateFieldRenames = []struct {
	pkg, typ, old, new string // pkg relative to the nexus module
}{
	{"middleware", "Middleware", "Gin", "HTTP"},
}

func migrateGoFields(_ string, src []byte) ([]byte, []migrateChange, error) {
	if !strings.Contains(string(src), "Gin") {
		return src, nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return src, nil, nil
	}
	// import name → package path relative to the nexus module
	pkgs := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		rel, ok := nexusRelPkg(p)
		if !ok {
			continue
		}
		name := rel[strings.LastIndex(rel, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		pkgs[name] = rel
	}
	type edit struct {
		at       int
		old, new string
		line     int
	}
	var edits []edit
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		for _, r := range migrateFieldRenames {
			if pkgs[x.Name] != r.pkg || sel.Sel.Name != r.typ {
				continue
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == r.old {
					pos := fset.Position(k.Pos())
					edits = append(edits, edit{pos.Offset, r.old, r.new, pos.Line})
				}
			}
		}
		return true
	})
	if len(edits) == 0 {
		return src, nil, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].at > edits[j].at })
	out := append([]byte(nil), src...)
	var changes []migrateChange
	for _, e := range edits {
		out = append(out[:e.at], append([]byte(e.new), out[e.at+len(e.old):]...)...)
		changes = append(changes, migrateChange{Line: e.line, Old: e.old + ":", New: e.new + ":"})
	}
	return out, changes, nil
}

// migrateGoSpreads drops the spread from calls that returned []Option in v1
// and return one Option in v2: append(opts, nexus.MustLoadExtensions()...).
var oneOptionSpread = regexp.MustCompile(`(\bMustLoadExtensions\([^()]*\))\.\.\.`)

func migrateGoSpreads(_ string, src []byte) ([]byte, []migrateChange, error) {
	if !oneOptionSpread.Match(src) {
		return src, nil, nil
	}
	var changes []migrateChange
	lines := strings.Split(string(src), "\n")
	for i, l := range lines {
		if nl := oneOptionSpread.ReplaceAllString(l, "$1"); nl != l {
			changes = append(changes, migrateChange{Line: i + 1, Old: strings.TrimSpace(l), New: strings.TrimSpace(nl)})
			lines[i] = nl
		}
	}
	return []byte(strings.Join(lines, "\n")), changes, nil
}
