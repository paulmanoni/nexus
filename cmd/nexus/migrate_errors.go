package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// migrateV2ErrorSymbols is the error-model part of the v1 → v2 symbol table
// (docs/design/v2.md §2): nexus.Errors became the InvalidInput form of
// nexus.Error, built with nexus.Invalid() (Field, Global, Any and First are
// unchanged); nexus.ErrForbidden is the nexus.Forbidden code, which is
// itself an error (return it, or match it with errors.Is); MapCRUDError has
// no replacement — every transport renders an error by its code.
var migrateV2ErrorSymbols = []migrateSymbol{
	{From: "", Old: "NewErrors", To: "", New: "Invalid"},
	{From: "", Old: "Errors", To: "", New: "Error"},
	{From: "", Old: "ErrForbidden", To: "", New: "Forbidden"},
	{From: "", Old: "MapCRUDError",
		Todo: "nexus.MapCRUDError is gone; errors render by code — return nexus.Err(nexus.NotFound, …) etc., or read nexus.ErrorOf(err).HTTPStatus()"},
}

// migrateGoErrors applies the error-model rewrites to a Go file. The boot
// option nexus.Error(err) became nexus.FailBoot(err) — nexus.Error is now
// the error type — so a one-argument call to it is renamed first (a call,
// never the type, which keeps the rewrite safe to run twice); the symbol
// rows follow.
func migrateGoErrors(_ string, src []byte) ([]byte, []migrateChange, error) {
	out, changes, err := renameErrorOption(src)
	if err != nil {
		return nil, nil, err
	}
	out2, more, err := rewriteGoSymbols(out, migrateV2ErrorSymbols)
	if err != nil {
		return nil, nil, err
	}
	return out2, append(changes, more...), nil
}

// renameErrorOption rewrites nexus.Error(x) calls to nexus.FailBoot(x).
func renameErrorOption(src []byte) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	root := map[string]bool{}
	for _, spec := range f.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		if rel, ok := nexusRelPkg(p); !ok || rel != "" {
			continue
		}
		name := "nexus"
		if spec.Name != nil {
			name = spec.Name.Name
		}
		root[name] = true
	}
	if len(root) == 0 {
		return src, nil, nil
	}
	var offs []int
	var changes []migrateChange
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Error" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil && root[id.Name] {
			offs = append(offs, fset.Position(sel.Sel.Pos()).Offset)
			changes = append(changes, migrateChange{
				Line: fset.Position(sel.Pos()).Line,
				Old:  id.Name + ".Error(…)",
				New:  id.Name + ".FailBoot(…)",
			})
		}
		return true
	})
	if len(offs) == 0 {
		return src, nil, nil
	}
	var b bytes.Buffer
	last := 0
	for _, at := range offs { // ast.Inspect visits in source order
		b.Write(src[last:at])
		b.WriteString("FailBoot")
		last = at + len("Error")
	}
	b.Write(src[last:])
	return b.Bytes(), changes, nil
}
