package viewgen

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
)

// Instrument makes templ's generated Go record a live page's render tree
// (view.Rec): each static is written through the component's recorder, each
// loop records its items, and each branch and nested component render opens
// a frame of its own. Outside a live page the recorder only writes, so the
// output is the same markup.
//
// It is idempotent: an instrumented file comes back as it is.
func Instrument(src []byte) ([]byte, error) {
	if bytes.Contains(src, []byte(recAlias+".Record(")) {
		return src, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isSel(call.Fun, "templruntime", "GeneratedTemplate") || len(call.Args) != 1 {
			return true
		}
		if fn, ok := call.Args[0].(*ast.FuncLit); ok && instrumentBody(fn.Body) {
			found = true
		}
		return true
	})
	if !found {
		return src, nil
	}
	addImport(file, recAlias, ViewImport)
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return nil, err
	}
	return format.Source(buf.Bytes())
}

const (
	recAlias = "templ_nx_view"
	recVar   = "templ_7745c5c3_Rec"
	bufVar   = "templ_7745c5c3_Buffer"
)

// instrumentBody declares the recorder after the component's preamble and
// instruments what follows.
func instrumentBody(body *ast.BlockStmt) bool {
	for i, st := range body.List {
		as, ok := st.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 {
			continue
		}
		if call, ok := as.Rhs[0].(*ast.CallExpr); ok && isSel(call.Fun, "templ", "InitializeContext") {
			decl := &ast.AssignStmt{
				Lhs: []ast.Expr{ast.NewIdent(recVar)},
				Tok: token.DEFINE,
				Rhs: []ast.Expr{&ast.CallExpr{Fun: sel(recAlias, "Record"), Args: []ast.Expr{ast.NewIdent("ctx"), ast.NewIdent(bufVar)}}},
			}
			use := &ast.AssignStmt{Lhs: []ast.Expr{ast.NewIdent("_")}, Tok: token.ASSIGN, Rhs: []ast.Expr{ast.NewIdent(recVar)}}
			rest := stmts(body.List[i+1:])
			body.List = append(append(append([]ast.Stmt{}, body.List[:i+1]...), decl, use), rest...)
			return true
		}
	}
	return false
}

// stmts instruments a statement list: loops, branches and component renders
// that write to the buffer, and every static write.
func stmts(list []ast.Stmt) []ast.Stmt {
	out := make([]ast.Stmt, 0, len(list))
	for _, st := range list {
		statics(st)
		if !writes(st) {
			out = append(out, st)
			continue
		}
		switch s := st.(type) {
		case *ast.RangeStmt:
			s.Body.List = append([]ast.Stmt{recCall("Item")}, stmts(s.Body.List)...)
			out = append(out, recCall("ForStart"), s, recCall("ForEnd"))
		case *ast.ForStmt:
			s.Body.List = append([]ast.Stmt{recCall("Item")}, stmts(s.Body.List)...)
			out = append(out, recCall("ForStart"), s, recCall("ForEnd"))
		case *ast.IfStmt:
			branches(s)
			out = append(out, recCall("Open"), s, recCall("Close"))
		case *ast.SwitchStmt:
			cases(s.Body)
			out = append(out, recCall("Open"), s, recCall("Close"))
		case *ast.TypeSwitchStmt:
			cases(s.Body)
			out = append(out, recCall("Open"), s, recCall("Close"))
		case *ast.BlockStmt:
			s.List = stmts(s.List)
			out = append(out, s)
		default:
			if rendersComponent(st) {
				out = append(out, recCall("Open"), st, recCall("Close"))
			} else {
				out = append(out, st)
			}
		}
	}
	return out
}

func branches(s *ast.IfStmt) {
	s.Body.List = stmts(s.Body.List)
	switch e := s.Else.(type) {
	case *ast.BlockStmt:
		e.List = stmts(e.List)
	case *ast.IfStmt:
		branches(e)
	}
}

func cases(body *ast.BlockStmt) {
	for _, c := range body.List {
		if cc, ok := c.(*ast.CaseClause); ok {
			cc.Body = stmts(cc.Body)
		}
	}
}

// statics rewrites templruntime.WriteString(buf, n, s) in st — outside
// nested components, which have recorders of their own — to rec.S(buf, n, s).
func statics(st ast.Stmt) {
	ast.Inspect(st, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if ok && isSel(call.Fun, "templruntime", "WriteString") && len(call.Args) == 3 && isIdent(call.Args[0], bufVar) {
			call.Fun = sel(recVar, "S")
		}
		return true
	})
}

// writes reports whether st writes to the component's buffer, outside nested
// components.
func writes(st ast.Stmt) bool {
	found := false
	ast.Inspect(st, func(n ast.Node) bool {
		if found {
			return false
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == bufVar {
			found = true
		}
		return true
	})
	return found
}

// rendersComponent reports whether st is templ_7745c5c3_Err = x.Render(…, buf).
func rendersComponent(st ast.Stmt) bool {
	as, ok := st.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return false
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 || !isIdent(call.Args[1], bufVar) {
		return false
	}
	fn, ok := call.Fun.(*ast.SelectorExpr)
	return ok && fn.Sel.Name == "Render"
}

func recCall(method string) ast.Stmt {
	return &ast.ExprStmt{X: &ast.CallExpr{Fun: sel(recVar, method), Args: []ast.Expr{ast.NewIdent(bufVar)}}}
}

func sel(x, name string) *ast.SelectorExpr {
	return &ast.SelectorExpr{X: ast.NewIdent(x), Sel: ast.NewIdent(name)}
}

func isSel(e ast.Expr, x, name string) bool {
	s, ok := e.(*ast.SelectorExpr)
	return ok && s.Sel.Name == name && isIdent(s.X, x)
}

// addImport adds alias "path" to the file's imports.
func addImport(file *ast.File, alias, path string) {
	spec := &ast.ImportSpec{Name: ast.NewIdent(alias), Path: &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(path)}}
	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			if !gd.Lparen.IsValid() {
				gd.Lparen, gd.Rparen = gd.Pos(), gd.End()
			}
			gd.Specs = append(gd.Specs, spec)
			file.Imports = append(file.Imports, spec)
			return
		}
	}
	gd := &ast.GenDecl{Tok: token.IMPORT, Lparen: 1, Specs: []ast.Spec{spec}}
	file.Decls = append([]ast.Decl{gd}, file.Decls...)
	file.Imports = append(file.Imports, spec)
}
