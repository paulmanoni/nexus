package viewgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// Instrument makes templ's generated Go record a live page's render tree
// (view.Rec): each static is written through the component's recorder, each
// loop records its items, and each branch and nested component render opens
// a frame of its own. Outside a live page the recorder only writes, so the
// output is the same markup.
//
// A part that reads none of the template's own variables — its parameters,
// loop variables, {{ }} locals — only its receiver, ctx and package-level
// names, is a spot: Guard opens it, and a live page that tracks its state
// (view.Assign) skips it when nothing it read changed.
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
	for _, d := range file.Decls {
		in := &instr{fset: fset, pkg: file.Name.Name}
		if fd, ok := d.(*ast.FuncDecl); ok {
			in.fn = fd
			in.name = fd.Name.Name
			if fd.Recv != nil && len(fd.Recv.List) == 1 {
				in.name = recvType(fd.Recv.List[0].Type) + "." + in.name
				if names := fd.Recv.List[0].Names; len(names) == 1 && names[0].Name != "_" {
					in.recv = names[0]
				}
			}
		}
		ast.Inspect(d, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isSel(call.Fun, "templruntime", "GeneratedTemplate") || len(call.Args) != 1 {
				return true
			}
			if fn, ok := call.Args[0].(*ast.FuncLit); ok && in.body(fn.Body) {
				found = true
			}
			return true
		})
	}
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

// instr instruments the components of one declaration.
type instr struct {
	fset  *token.FileSet
	pkg   string
	fn    *ast.FuncDecl // the declaration, when it is a function
	name  string        // its name, Type.Method for a method
	recv  *ast.Ident    // its receiver, if named
	spots int
}

func recvType(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return recvType(t.X)
	case *ast.IndexExpr:
		return recvType(t.X)
	case *ast.IndexListExpr:
		return recvType(t.X)
	case *ast.Ident:
		return t.Name
	}
	return "?"
}

// body declares the recorder after the component's preamble and
// instruments what follows.
func (in *instr) body(body *ast.BlockStmt) bool {
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
			rest := in.stmts(body.List[i+1:])
			body.List = append(append(append([]ast.Stmt{}, body.List[:i+1]...), decl, use), rest...)
			return true
		}
	}
	return false
}

// stmts instruments a statement list: loops, branches and component renders
// that write to the buffer, and every static write.
func (in *instr) stmts(list []ast.Stmt) []ast.Stmt {
	out := make([]ast.Stmt, 0, len(list))
	for _, st := range list {
		statics(st)
		if !writes(st) {
			out = append(out, st)
			continue
		}
		switch s := st.(type) {
		case *ast.RangeStmt:
			guard := in.guard(s)
			s.Body.List = append([]ast.Stmt{recCall("Item")}, in.stmts(s.Body.List)...)
			out = append(out, in.wrap(guard, recCall("ForStart"), s, recCall("ForEnd"))...)
		case *ast.ForStmt:
			guard := in.guard(s)
			s.Body.List = append([]ast.Stmt{recCall("Item")}, in.stmts(s.Body.List)...)
			out = append(out, in.wrap(guard, recCall("ForStart"), s, recCall("ForEnd"))...)
		case *ast.IfStmt:
			guard := in.guard(s)
			in.branches(s)
			out = append(out, in.frame(guard, s)...)
		case *ast.SwitchStmt:
			guard := in.guard(s)
			in.cases(s.Body)
			out = append(out, in.frame(guard, s)...)
		case *ast.TypeSwitchStmt:
			guard := in.guard(s)
			in.cases(s.Body)
			out = append(out, in.frame(guard, s)...)
		case *ast.BlockStmt:
			s.List = in.stmts(s.List)
			out = append(out, s)
		default:
			if rendersComponent(st) {
				out = append(out, in.frame(in.guard(st), st)...)
			} else {
				out = append(out, st)
			}
		}
	}
	return out
}

// frame opens a frame around st: Open, or Guard for a spot.
func (in *instr) frame(guard ast.Expr, st ast.Stmt) []ast.Stmt {
	if guard == nil {
		return []ast.Stmt{recCall("Open"), st, recCall("Close")}
	}
	return in.wrap(guard, st)
}

// wrap puts a spot's statements under its Guard: they run when it reports
// true, and Close ends the spot either way.
func (in *instr) wrap(guard ast.Expr, body ...ast.Stmt) []ast.Stmt {
	if guard == nil {
		return body
	}
	return []ast.Stmt{&ast.IfStmt{Cond: guard, Body: &ast.BlockStmt{List: body}}, recCall("Close")}
}

func (in *instr) branches(s *ast.IfStmt) {
	s.Body.List = in.stmts(s.Body.List)
	switch e := s.Else.(type) {
	case *ast.BlockStmt:
		e.List = in.stmts(e.List)
	case *ast.IfStmt:
		in.branches(e)
	}
}

func (in *instr) cases(body *ast.BlockStmt) {
	for _, c := range body.List {
		if cc, ok := c.(*ast.CaseClause); ok {
			cc.Body = in.stmts(cc.Body)
		}
	}
}

// guard is the Guard call for st when it is a spot, else nil.
func (in *instr) guard(st ast.Stmt) ast.Expr {
	if in.fn == nil {
		return nil
	}
	recv, ok := in.free(st, map[*ast.FuncLit]bool{})
	if !ok {
		return nil
	}
	in.spots++
	label := fmt.Sprintf("%s.%s#%d", in.pkg, in.name, in.spots)
	if at := templAt(st); at != "" {
		label += " (" + at + ")"
	}
	var r ast.Expr = ast.NewIdent("nil")
	if recv {
		r = ast.NewIdent(in.recv.Name)
	}
	return &ast.CallExpr{Fun: sel(recVar, "Guard"), Args: []ast.Expr{
		ast.NewIdent("ctx"), ast.NewIdent(bufVar), &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(label)}, r,
	}}
}

// templInternal are the generated code's own variables a spot may use.
var templInternal = map[string]bool{
	"ctx": true, bufVar: true, recVar: true, "templ_7745c5c3_Err": true, "templ_7745c5c3_W": true, "templ_7745c5c3_Input": true,
}

// free reports whether n uses no variable of the declaration from outside
// itself but its receiver (recv: it does), ctx, the generated code's own,
// and the children components it renders — which follow the same rule.
func (in *instr) free(n ast.Node, seen map[*ast.FuncLit]bool) (recv, ok bool) {
	ok = true
	ast.Inspect(n, func(x ast.Node) bool {
		if !ok {
			return false
		}
		id, isIdent := x.(*ast.Ident)
		if !isIdent || id.Obj == nil || id.Obj.Kind != ast.Var {
			return true
		}
		at := id.Obj.Pos()
		if !at.IsValid() || (at >= n.Pos() && at < n.End()) || at < in.fn.Pos() || at >= in.fn.End() {
			return true // its own, or package-level
		}
		switch {
		case in.recv != nil && id.Obj == in.recv.Obj:
			recv = true
		case templInternal[id.Name]:
		default:
			lit := childrenOf(id.Obj)
			if lit == nil {
				ok = false
				return false
			}
			if !seen[lit] {
				seen[lit] = true
				r, good := in.free(lit, seen)
				recv = recv || r
				ok = good
			}
		}
		return ok
	})
	return recv, ok
}

// childrenOf returns the component a templ_7745c5c3_VarN := templruntime.
// GeneratedTemplate(func…) declared — the children a call passes on.
func childrenOf(obj *ast.Object) *ast.FuncLit {
	if !strings.HasPrefix(obj.Name, "templ_7745c5c3_Var") {
		return nil
	}
	as, ok := obj.Decl.(*ast.AssignStmt)
	if !ok || len(as.Rhs) != 1 {
		return nil
	}
	call, ok := as.Rhs[0].(*ast.CallExpr)
	if !ok || !isSel(call.Fun, "templruntime", "GeneratedTemplate") || len(call.Args) != 1 {
		return nil
	}
	lit, _ := call.Args[0].(*ast.FuncLit)
	return lit
}

// templAt is the template position of the first expression in st templ
// reports errors at: file:line.
func templAt(st ast.Node) string {
	at := ""
	ast.Inspect(st, func(n ast.Node) bool {
		if at != "" {
			return false
		}
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isSel(cl.Type, "templ", "Error") {
			return true
		}
		var file, line string
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			k, _ := kv.Key.(*ast.Ident)
			v, _ := kv.Value.(*ast.BasicLit)
			if k == nil || v == nil {
				continue
			}
			switch k.Name {
			case "FileName":
				file, _ = strconv.Unquote(v.Value)
			case "Line":
				line = v.Value
			}
		}
		if file != "" {
			at = file + ":" + line
		}
		return false
	})
	return at
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
