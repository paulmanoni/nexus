package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// v1 derived an op's name from its handler with any New prefix dropped
// (NewListPets → listPets); v2 uses the name as written. migrateGoOpNames
// keeps every v1 wire name: a registration whose handler is NewXxx gets
// nexus.Op("xxx"), and an annotated NewXxx gets //nexus:use nexus.Op("xxx").
// Registrations that already name their op are left alone.
var registrationHandlerArg = map[string]int{
	"AsQuery": 0, "AsMutation": 0, "AsSubscription": 0,
	"AsRest": 2, "AsWS": 2,
}

var opDirective = regexp.MustCompile(`^//nexus:(rest|query|mutation|subscription|ws)\b`)

func migrateGoOpNames(_ string, src []byte) ([]byte, []migrateChange, error) {
	if !strings.Contains(string(src), "New") {
		return src, nil, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return src, nil, nil // not ours to report; the build will
	}
	nexusName := nexusImportName(f)

	type insert struct {
		at   int
		text string
	}
	var inserts []insert
	var changes []migrateChange

	// Registrations: nexus.AsQuery(NewXxx, …).
	if nexusName != "" {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != nexusName {
				return true
			}
			i, ok := registrationHandlerArg[sel.Sel.Name]
			if !ok || i >= len(call.Args) {
				return true
			}
			name := handlerName(call.Args[i])
			op, ok := v1OpName(name)
			if !ok || callsOp(call, nexusName) {
				return true
			}
			text := ", " + nexusName + ".Op(" + strconv.Quote(op) + ")"
			inserts = append(inserts, insert{fset.Position(call.Args[i].End()).Offset, text})
			changes = append(changes, migrateChange{Line: fset.Position(call.Pos()).Line, Old: name, New: name + text})
			return true
		})
	}

	// Annotated handlers: //nexus:query above func NewXxx.
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Doc == nil || fn.Recv != nil {
			continue
		}
		op, ok := v1OpName(fn.Name.Name)
		if !ok {
			continue
		}
		var last *ast.Comment
		named := false
		for _, c := range fn.Doc.List {
			if opDirective.MatchString(c.Text) {
				last = c
			}
			if strings.Contains(c.Text, ".Op(") {
				named = true
			}
		}
		if last == nil || named {
			continue
		}
		text := "\n//nexus:use nexus.Op(" + strconv.Quote(op) + ")"
		inserts = append(inserts, insert{fset.Position(last.End()).Offset, text})
		changes = append(changes, migrateChange{Line: fset.Position(last.Pos()).Line, Old: last.Text, New: strings.TrimSpace(text)})
	}

	if len(inserts) == 0 {
		return src, nil, nil
	}
	sort.Slice(inserts, func(a, b int) bool { return inserts[a].at > inserts[b].at })
	out := append([]byte(nil), src...)
	for _, in := range inserts {
		out = append(out[:in.at], append([]byte(in.text), out[in.at:]...)...)
	}
	return out, changes, nil
}

// v1OpName is the op name v1 gave a NewXxx handler: Xxx, first letter
// lowered. ok is false for any other name — its v2 name is the same.
func v1OpName(name string) (string, bool) {
	if len(name) < 4 || !strings.HasPrefix(name, "New") || !unicode.IsUpper(rune(name[3])) {
		return "", false
	}
	rest := name[3:]
	return strings.ToLower(rest[:1]) + rest[1:], true
}

func handlerName(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func callsOp(call *ast.CallExpr, nexusName string) bool {
	for _, a := range call.Args {
		c, ok := a.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Op" {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == nexusName {
				return true
			}
		}
	}
	return false
}

// nexusImportName is the name the file imports the nexus root package as
// (v1 or v2 path), or "" when it doesn't.
func nexusImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != "github.com/paulmanoni/nexus" && p != "github.com/paulmanoni/nexus/v2" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "nexus"
	}
	return ""
}
