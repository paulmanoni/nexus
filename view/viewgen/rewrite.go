// Package rewrite compiles reactive .templ files. The source is plain templ;
// what makes it reactive is which values it reads:
//
//	{{ count := view.State(ctx, 0) }}                 a signal owned by the component
//	<p>Clicked { count.Get() } times</p>              text reading a signal: kept current
//	<button disabled?={ count.Get() == 0 }>           attribute reading a signal: kept current
//	<button onclick={ count.Set(count.Get() + 1) }>   on* attribute with an action: runs in the browser
//	if count.Get() >= 5 { … }                         if on a signal: the browser shows the branch
//	{{ pets := search(ctx, q.Get()) }}                server code reading a signal: the component is a shard
//
// Directives above a component — //nexus:page METHOD PATH, //nexus:auth …, //nexus:use … —
// register it; see package.go.
//
// The pass parses a file with templ's parser, finds the signals of each
// component (view.State locals and *view.Signal parameters), compiles every
// browser-side expression to JavaScript (internal/jsgen), rewrites those
// nodes into runtime calls, and hands the tree to templ's generator. Because
// the source stays plain templ, templ's editor tooling works on it as is.
package viewgen

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/a-h/parse"
	"github.com/a-h/templ/generator"
	"github.com/a-h/templ/parser/v2"

	"github.com/paulmanoni/nexus/v2/view/viewgen/jsgen"
)

// ViewImport is the runtime package generated code calls.
const ViewImport = "github.com/paulmanoni/nexus/v2/view"

// Result is one compiled .templ file.
type Result struct {
	Package    string
	Go         []byte            // the *_templ.go source
	Twins      map[string]string // compiled expressions by id
	Components []*Component
	Imports    map[string]string // the file's imports: selector → import line

	// RawGo is the Go as templ's generator wrote it, before gofmt and the
	// live render's recorder calls (Instrument): what
	// SourceMap indexes. An editor type-checks RawGo so a .templ position
	// maps onto it, and back, through SourceMap.
	RawGo     []byte
	SourceMap *parser.SourceMap

	// Warnings don't stop the build (track.go); Pages are the types whose
	// method components it compiled that use view.Assign.
	Warnings []*PositionError
	Pages    map[string]bool
}

// Component is what the package pass needs to know about one component.
type Component struct {
	ID       string // import path + "." + Name
	Name     string
	At       *PositionError // where it is declared (Msg empty)
	Params   int
	Method   string // from //nexus:page; empty when not a page
	Path     string
	Gates    []string // Go options from //nexus:auth and //nexus:use
	HasGates bool
	Calls    []string // IDs of the components it renders
	Shard    bool
	Uses     []string          // types it reads with view.Use
	Imports  map[string]string // its file's imports, for its gates
}

// Package is what the Go files of a package declare, read by Scan.
type Package struct {
	Shards map[string]bool     // registered in Go with view.Shard
	Pages  map[string]bool     // registered in Go with view.Page
	States map[string][]string // state structs: type name → its signal fields
	// ImportPath is the package's import path ("" in single-package use).
	ImportPath string
	// Lookup returns the signal fields of state type typ in another package.
	Lookup  func(importPath, typ string) []string
	Exposed map[string]bool    // types exposed in Go with view.Expose
	Structs map[string]*Fields // struct types that have view.Assign fields
	// AllStructs is every struct type, for the structs a page embeds.
	AllStructs map[string]*Fields
	// LookupStruct returns the fields of struct type typ in another package
	// of the module, its embedded structs' included; nil when unknown.
	LookupStruct func(importPath, typ string) *Fields
	// Live has the generated Go record a live page's render tree
	// (Instrument): set for a module that depends on nexus, whose view
	// package the recorder calls.
	Live bool
}

// PositionError is a compile error at a line and column of a .templ file.
type PositionError struct {
	File      string
	Line, Col int // 1-based
	Msg       string
}

func (e *PositionError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s", e.File, e.Line, e.Col, e.Msg)
}

// File compiles one .templ file of a package whose Go files declare pkg.
func File(name, src string, pkg *Package) (*Result, error) {
	if pkg == nil {
		pkg = &Package{}
	}
	tf, err := parser.ParseString(src)
	if err != nil {
		var pe parse.ParseError
		if errors.As(err, &pe) {
			return nil, &PositionError{File: filepath.ToSlash(name), Line: pe.Pos.Line + 1, Col: pe.Pos.Col + 1, Msg: pe.Msg}
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	tf.Filepath = name
	f := &fileRewriter{
		file:       filepath.ToSlash(name),
		twins:      map[string]string{},
		pkg:        pkg,
		importView: strings.Contains(src, strconv.Quote(ViewImport)),
		imports:    map[string]string{},
		livePages:  map[string]bool{},
	}
	var doc string
	var docAt parser.Position
	for _, n := range tf.Nodes {
		switch n := n.(type) {
		case *parser.TemplateFileGoExpression:
			f.collectImports(n.Expression.Value)
			doc, docAt = n.Expression.Value, n.Expression.Range.From
		case *parser.HTMLTemplate:
			f.template(n, doc, docAt)
			doc = ""
		default:
			doc = ""
		}
	}
	if len(f.errs) > 0 {
		return nil, errors.Join(f.errs...)
	}
	var buf bytes.Buffer
	gen, err := generator.Generate(tf, &buf, generator.WithFileName(filepath.Base(name)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	out, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%s: generated Go does not parse: %w", name, err)
	}
	if pkg.Live && pkg.ImportPath != ViewImport {
		if out, err = Instrument(out); err != nil {
			return nil, fmt.Errorf("%s: instrumenting the generated Go: %w", name, err)
		}
	}
	return &Result{
		Package: strings.TrimSpace(strings.TrimPrefix(tf.Package.Expression.Value, "package")),
		Go:      out, Twins: f.twins, Components: f.components, Imports: f.imports,
		RawGo: buf.Bytes(), SourceMap: gen.SourceMap,
		Warnings: f.warnings, Pages: f.livePages,
	}, nil
}

type fileRewriter struct {
	file       string
	twins      map[string]string
	pkg        *Package
	importView bool
	imports    map[string]string
	components []*Component
	errs       []error
	warnings   []*PositionError
	livePages  map[string]bool
}

// collectImports records the import lines of a file-level Go block.
func (f *fileRewriter) collectImports(code string) {
	if !strings.Contains(code, "import") {
		return
	}
	file, err := goparser.ParseFile(token.NewFileSet(), "", "package p\n"+code, goparser.ImportsOnly)
	if err != nil {
		return
	}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		sel, line := path[strings.LastIndexByte(path, '/')+1:], spec.Path.Value
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			sel, line = spec.Name.Name, spec.Name.Name+" "+spec.Path.Value
		}
		f.imports[sel] = line
	}
}

func (f *fileRewriter) fail(pos parser.Position, format string, args ...any) {
	f.errs = append(f.errs, &PositionError{File: f.file, Line: int(pos.Line) + 1, Col: int(pos.Col) + 1, Msg: fmt.Sprintf(format, args...)})
}

// failExpr reports err from compiling expr, at the construct's own position
// when jsgen gives one.
func (f *fileRewriter) failExpr(at parser.Expression, err error, prefix string) {
	line, col := int(at.Range.From.Line)+1, int(at.Range.From.Col)+1
	var je *jsgen.Error
	if errors.As(err, &je) && je.Offset >= 0 && je.Offset <= len(at.Value) {
		before := at.Value[:je.Offset]
		if i := strings.LastIndexByte(before, '\n'); i >= 0 {
			line, col = line+strings.Count(before, "\n"), je.Offset-i
		} else {
			col += je.Offset
		}
	}
	f.errs = append(f.errs, &PositionError{File: f.file, Line: line, Col: col, Msg: prefix + err.Error()})
}

func (f *fileRewriter) id(kind string, pos parser.Position, extra string) string {
	h := sha256.Sum256([]byte(f.file + ":" + strconv.Itoa(int(pos.Line)) + ":" + strconv.Itoa(int(pos.Col)) + ":" + kind + extra))
	return kind + hex.EncodeToString(h[:5])
}

// component is one templ component being rewritten.
type component struct {
	*fileRewriter
	name     string
	params   []string
	signals  map[string]bool // every signal name in the component
	topLevel map[string]bool // signals declared at its top level, or parameters
	paths    jsgen.Paths     // signal fields of state structs it Uses: "s.Query"
	reads    []string        // signals read by server code, in first-read order
	readAt   parser.Position // where the first server read is
	uses     bool            // uses any reactive feature
	calls    []string        // components it renders
	method   bool            // a method component (a live page's Render)
	recvName string          // a method component's receiver, and its type
	recvType string
}

func (f *fileRewriter) template(t *parser.HTMLTemplate, doc string, docAt parser.Position) {
	c := &component{fileRewriter: f, signals: map[string]bool{}, topLevel: map[string]bool{}, paths: jsgen.Paths{}}
	if !c.signature(t) {
		return
	}
	top := map[*parser.GoCode]bool{}
	for _, n := range t.Children {
		if g, ok := n.(*parser.GoCode); ok {
			top[g] = true
		}
	}
	walkGoCode(t.Children, func(g *parser.GoCode) {
		sigs, holders := f.stateLocals(g.Expression.Value)
		for _, s := range sigs {
			c.signals[s] = true
			c.topLevel[s] = c.topLevel[s] || top[g]
		}
		for name, fields := range holders {
			for _, field := range fields {
				p := name + "." + field
				c.paths[p] = true
				c.topLevel[p] = c.topLevel[p] || top[g]
			}
		}
	})
	info := &Component{ID: qualify(f.pkg.ImportPath, c.name), Name: c.name, Params: len(c.params), Imports: f.imports,
		At: &PositionError{File: f.file, Line: int(t.Range.From.Line) + 1, Col: int(t.Range.From.Col) + 1}}
	if !f.directives(info, doc, docAt, t.Range.From) {
		return
	}
	info.Uses = usedTypes(t)
	c.trackWarnings(t)
	t.Children = c.nodes(t.Children)
	if c.method {
		if info.Method != "" || info.HasGates {
			f.fail(t.Range.From, "%s is a method component: register its type with view.Live, and gate it there", c.name)
			return
		}
		if len(c.reads) > 0 {
			f.fail(c.readAt, "%s reads %s on the server — a live page keeps server data in its own fields; read the signal in the browser, or keep the value in the struct", c.name, c.reads[0])
			return
		}
	}
	shard := len(c.reads) > 0 || f.pkg.Shards[c.name]
	info.Shard, info.Calls = shard, c.calls
	f.components = append(f.components, info)
	if (c.uses || shard || len(c.signals) > 0) && !f.importView {
		f.fail(t.Range.From, "%s is reactive — import %q", c.name, ViewImport)
		return
	}
	if !f.importView {
		return
	}
	var pre, post []parser.Node
	pre = append(pre, &parser.GoCode{Expression: parser.Expression{Value: fmt.Sprintf("ctx = view.Enter(ctx, %q)", c.name)}})
	if shard {
		for _, r := range c.reads {
			if !c.topLevel[r] {
				f.fail(c.readAt, "%s reads %s on the server — declare it at the top of %s so the shard can watch it", c.name, r, c.name)
				return
			}
		}
		pre = append(pre, &parser.TemplElementExpression{Expression: parser.Expression{Value: "view.ShardStart()"}})
		post = append(post, &parser.TemplElementExpression{Expression: parser.Expression{
			Value: fmt.Sprintf("view.ShardEnd(%s, []any{%s}, []any{%s})", c.name, strings.Join(c.params, ", "), strings.Join(c.reads, ", ")),
		}})
	}
	t.Children = append(append(pre, t.Children...), post...)
}

// signature reads the component's name and parameters, noting which
// parameters are signals.
func (c *component) signature(t *parser.HTMLTemplate) bool {
	src := "package p\nfunc " + t.Expression.Value + " {}"
	file, err := goparser.ParseFile(token.NewFileSet(), "", src, 0)
	if err != nil || len(file.Decls) != 1 {
		c.fail(t.Range.From, "cannot read the component signature %q", t.Expression.Value)
		return false
	}
	fd := file.Decls[0].(*ast.FuncDecl)
	c.name = fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		// A method component — a live page's Render. Its scope is named
		// after the type and the method.
		recv := fd.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if generic := recvType(recv); generic != "?" && generic != "" {
			// A generic type's too (Page[R, I]), for its warnings.
			c.recvType = generic
			if names := fd.Recv.List[0].Names; len(names) == 1 && names[0].Name != "_" {
				c.recvName = names[0].Name
			}
		}
		if id, ok := recv.(*ast.Ident); ok {
			c.name = id.Name + "." + fd.Name.Name
			c.method = true
		}
	}
	for _, p := range fd.Type.Params.List {
		isSignal := strings.Contains(src[p.Type.Pos()-1:p.Type.End()-1], "view.Signal")
		for _, n := range p.Names {
			c.params = append(c.params, n.Name)
			if isSignal {
				c.signals[n.Name] = true
				c.topLevel[n.Name] = true
			}
		}
	}
	return true
}

// stateLocals returns the names a {{ }} block binds to a signal — with
// view.State, or a signal field of a state struct (view.Use[*T](ctx).F) —
// and the names it binds to a whole state struct (view.Use[*T](ctx)), with
// that struct's signal fields.
func (f *fileRewriter) stateLocals(code string) ([]string, map[string][]string) {
	body := parseStmts(code)
	holders := map[string][]string{}
	if body == nil {
		return nil, holders
	}
	var sigs []string
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Rhs) != len(as.Lhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			id, ok := as.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			if isStateCall(rhs) {
				sigs = append(sigs, id.Name)
				continue
			}
			if fields := f.usedState(rhs); fields != nil {
				holders[id.Name] = fields
				continue
			}
			if sel, ok := rhs.(*ast.SelectorExpr); ok && contains(f.usedState(sel.X), sel.Sel.Name) {
				sigs = append(sigs, id.Name)
			}
		}
		return true
	})
	return sigs, holders
}

// usedState returns the signal fields of T when e is view.Use[*T](…) for a
// state struct T — in this package, or another one (view.Use[*state.T]).
func (f *fileRewriter) usedState(e ast.Expr) []string {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return nil
	}
	ix, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return nil
	}
	sel, ok := ix.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Use" || !isIdent(sel.X, "view") {
		return nil
	}
	star, ok := ix.Index.(*ast.StarExpr)
	if !ok {
		return nil
	}
	switch t := star.X.(type) {
	case *ast.Ident:
		return f.pkg.States[t.Name]
	case *ast.SelectorExpr:
		pkg, ok := t.X.(*ast.Ident)
		if !ok || f.pkg.Lookup == nil {
			return nil
		}
		if path := importPathOf(f.imports[pkg.Name]); path != "" {
			return f.pkg.Lookup(path, t.Sel.Name)
		}
	}
	return nil
}

// importPathOf reads the path from an import line: "p" or alias "p".
func importPathOf(line string) string {
	if i := strings.IndexByte(line, '"'); i >= 0 {
		p, _ := strconv.Unquote(line[i:])
		return p
	}
	return ""
}

func qualify(importPath, name string) string {
	if importPath == "" {
		return name
	}
	return importPath + "." + name
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func isStateCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	fun := call.Fun
	if ix, ok := fun.(*ast.IndexExpr); ok {
		fun = ix.X
	}
	sel, ok := fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "State" && isIdent(sel.X, "view")
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func parseStmts(code string) *ast.BlockStmt {
	file, err := goparser.ParseFile(token.NewFileSet(), "", "package p\nfunc _() {\n"+code+"\n}", 0)
	if err != nil || len(file.Decls) != 1 {
		return nil
	}
	return file.Decls[0].(*ast.FuncDecl).Body
}

func walkGoCode(ns []parser.Node, fn func(*parser.GoCode)) {
	for _, n := range ns {
		if g, ok := n.(*parser.GoCode); ok {
			fn(g)
		}
		if cn, ok := n.(parser.CompositeNode); ok {
			walkGoCode(cn.ChildNodes(), fn)
		}
	}
}

// signalReads lists the signals node reads with Get.
func (c *component) signalReads(node ast.Node) []string {
	var out []string
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if isViewCall(call, "Attrs") {
			return false // compiled attributes: read in the browser, not on the server
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Get" {
			if id, ok := sel.X.(*ast.Ident); ok && c.signals[id.Name] {
				out = append(out, id.Name)
			} else if p := jsgen.SelectorPath(sel.X); c.paths[p] {
				out = append(out, p)
			}
		}
		return true
	})
	return out
}

func (c *component) exprReads(expr string) []string {
	e, err := goparser.ParseExpr(expr)
	if err != nil {
		return nil
	}
	return c.signalReads(e)
}

// serverRead records signals read by code that runs on the server.
func (c *component) serverRead(names []string, pos parser.Position) {
	for _, n := range names {
		seen := false
		for _, r := range c.reads {
			seen = seen || r == n
		}
		if !seen {
			if len(c.reads) == 0 {
				c.readAt = pos
			}
			c.reads = append(c.reads, n)
		}
	}
}

func capsLiteral(expr string, paths jsgen.Paths) (string, error) {
	caps, err := jsgen.CapturesIn(expr, paths)
	if err != nil {
		return "", err
	}
	if len(caps) == 0 {
		return "nil", nil
	}
	parts := make([]string, len(caps))
	for i, name := range caps {
		parts[i] = strconv.Quote(name) + ": " + name
	}
	return "view.Caps{" + strings.Join(parts, ", ") + "}", nil
}

// compile compiles a browser expression: its JavaScript and captures.
func (c *component) compile(expr string, handler bool) (js, caps string, err error) {
	if handler {
		js, err = jsgen.HandlerIn(expr, c.paths)
	} else {
		js, err = jsgen.BindIn(expr, c.paths)
	}
	if err != nil {
		return "", "", err
	}
	caps, err = capsLiteral(expr, c.paths)
	return js, caps, err
}

func (c *component) nodes(ns []parser.Node) []parser.Node {
	out := make([]parser.Node, 0, len(ns))
	for _, n := range ns {
		out = append(out, c.node(n)...)
	}
	return out
}

func (c *component) node(n parser.Node) []parser.Node {
	switch n := n.(type) {
	case *parser.Element:
		c.element(n)
		n.Children = c.nodes(n.Children)
	case *parser.StringExpression:
		expr := strings.TrimSpace(n.Expression.Value)
		if len(c.exprReads(expr)) == 0 {
			return []parser.Node{n}
		}
		js, caps, err := c.compile(expr, false)
		if err != nil {
			c.failExpr(n.Expression, err, "this text reads a signal, so it runs in the browser: ")
			return []parser.Node{n}
		}
		id := c.id("t", n.Expression.Range.From, "")
		c.twins[id] = js
		c.uses = true
		out := []parser.Node{&parser.TemplElementExpression{Expression: parser.Expression{
			Value: fmt.Sprintf("view.Text(%q, %s, %s)", id, caps, expr), Range: n.Expression.Range,
		}}}
		if n.TrailingSpace != parser.SpaceNone {
			out = append(out, &parser.Text{Value: " "})
		}
		return out
	case *parser.IfExpression:
		return c.ifExpr(n)
	case *parser.ForExpression:
		if body := parseStmts("for " + n.Expression.Value + " {}"); body != nil {
			c.serverRead(c.signalReads(body), n.Expression.Range.From)
		}
		n.Children = c.nodes(n.Children)
	case *parser.SwitchExpression:
		if body := parseStmts("switch " + n.Expression.Value + " {}"); body != nil {
			c.serverRead(c.signalReads(body), n.Expression.Range.From)
		}
		for i := range n.Cases {
			n.Cases[i].Children = c.nodes(n.Cases[i].Children)
		}
	case *parser.GoCode:
		c.attrLiterals(&n.Expression, true)
		if body := parseStmts(n.Expression.Value); body != nil {
			c.serverRead(c.signalReads(body), n.Expression.Range.From)
		}
	case *parser.TemplElementExpression:
		c.attrLiterals(&n.Expression, false)
		c.serverRead(c.exprReads(n.Expression.Value), n.Expression.Range.From)
		if e, err := goparser.ParseExpr(n.Expression.Value); err == nil {
			if call, ok := e.(*ast.CallExpr); ok {
				switch fn := call.Fun.(type) {
				case *ast.Ident:
					c.calls = append(c.calls, qualify(c.pkg.ImportPath, fn.Name))
				case *ast.SelectorExpr:
					if pkg, ok := fn.X.(*ast.Ident); ok {
						if path := importPathOf(c.imports[pkg.Name]); path != "" {
							c.calls = append(c.calls, qualify(path, fn.Sel.Name))
						}
					}
				}
			}
		}
		n.Children = c.nodes(n.Children)
	}
	return []parser.Node{n}
}

func (c *component) element(el *parser.Element) {
	for i, a := range el.Attributes {
		var key string
		var expr parser.Expression
		boolAttr := false
		switch a := a.(type) {
		case *parser.ExpressionAttribute:
			key, expr = a.Key.String(), a.Expression
		case *parser.BoolExpressionAttribute:
			key, expr, boolAttr = a.Key.String(), a.Expression, true
		default:
			continue
		}
		src := strings.TrimSpace(expr.Value)
		e, err := goparser.ParseExpr(src)
		if err != nil {
			continue
		}
		if !boolAttr && strings.HasPrefix(key, "on") && jsgen.IsAction(e) {
			js, caps, err := c.compile(src, true)
			if err != nil {
				c.failExpr(expr, err, "")
				continue
			}
			id := c.id("h", expr.Range.From, key)
			c.twins[id] = js
			c.uses = true
			el.Attributes[i] = &parser.SpreadAttributes{Expression: parser.Expression{
				Value: fmt.Sprintf("view.OnAttr(%q, %q, %s, %s)", key[2:], id, caps, src), Range: expr.Range,
			}}
			continue
		}
		if len(c.signalReads(e)) == 0 {
			continue
		}
		js, caps, err := c.compile(src, false)
		if err != nil {
			c.failExpr(expr, err, "this attribute reads a signal, so it runs in the browser: ")
			continue
		}
		id := c.id("b", expr.Range.From, key)
		c.twins[id] = js
		c.uses = true
		el.Attributes[i] = &parser.SpreadAttributes{Expression: parser.Expression{
			Value: fmt.Sprintf("view.Bind(%q, %q, %s, %s)", key, id, caps, src), Range: expr.Range,
		}}
	}
}

// ifExpr renders every branch of an if whose conditions read a signal, each
// shown only while its condition is the first to hold. An if on server
// values stays a plain templ if.
func (c *component) ifExpr(n *parser.IfExpression) []parser.Node {
	conds := []parser.Expression{n.Expression}
	branches := [][]parser.Node{n.Then}
	for _, ei := range n.ElseIfs {
		conds = append(conds, ei.Expression)
		branches = append(branches, ei.Then)
	}
	reads := false
	for _, cond := range conds {
		reads = reads || len(c.exprReads(cond.Value)) > 0
	}
	if !reads {
		n.Then = c.nodes(n.Then)
		for i := range n.ElseIfs {
			n.ElseIfs[i].Then = c.nodes(n.ElseIfs[i].Then)
		}
		n.Else = c.nodes(n.Else)
		return []parser.Node{n}
	}
	if len(n.Else) > 0 {
		branches = append(branches, n.Else)
	}
	var out []parser.Node
	var negated []string
	for i, branch := range branches {
		parts := append([]string(nil), negated...)
		if i < len(conds) {
			cond := strings.TrimSpace(conds[i].Value)
			if _, err := jsgen.BindIn(cond, c.paths); err != nil {
				c.failExpr(conds[i], err, "this if reads a signal, so the browser decides it: ")
				return []parser.Node{n}
			}
			parts = append(parts, "("+cond+")")
			negated = append(negated, "!("+cond+")")
		}
		show := strings.Join(parts, " && ")
		js, caps, err := c.compile(show, false)
		if err != nil {
			c.failExpr(n.Expression, err, "this if reads a signal, so the browser decides it: ")
			return []parser.Node{n}
		}
		id := c.id("w", n.Expression.Range.From, strconv.Itoa(i))
		c.twins[id] = js
		c.uses = true
		out = append(out, &parser.TemplElementExpression{
			Expression: parser.Expression{Value: fmt.Sprintf("view.When(%q, %s, %s)", id, caps, show), Range: n.Expression.Range},
			Children:   c.nodes(branch),
		})
	}
	return out
}

// attrLiterals compiles the reactive entries of every templ.Attributes{…}
// literal in expr — the attributes a component library takes in its Props
// — the way attributes on an element compile:
//
//	@button.Button(button.Props{Attributes: templ.Attributes{
//	    "onclick":  count.Set(count.Get() + 1),   // an action: runs in the browser
//	    "disabled": count.Get() >= 10,            // reads a signal: kept current
//	}})
//
// A literal with reactive entries becomes view.Attrs(static, parts…).
// stmts says whether expr holds statements ({{ }}) or one expression.
func (c *component) attrLiterals(expr *parser.Expression, stmts bool) {
	prefix, suffix := "package p\nvar _ = ", "\n"
	if stmts {
		prefix, suffix = "package p\nfunc _() {\n", "\n}\n"
	}
	full := prefix + expr.Value + suffix
	file, err := goparser.ParseFile(token.NewFileSet(), "", full, 0)
	if err != nil {
		return
	}
	type edit struct {
		from, to int
		text     string
	}
	var edits []edit
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Attributes" || !isIdent(sel.X, "templ") {
			return true
		}
		if text, ok := c.attrLiteral(lit, full, *expr, len(prefix)); ok {
			edits = append(edits, edit{int(lit.Pos()) - 1 - len(prefix), int(lit.End()) - 1 - len(prefix), text})
		}
		return false
	})
	src := expr.Value
	for i := len(edits) - 1; i >= 0; i-- {
		e := edits[i]
		src = src[:e.from] + e.text + src[e.to:]
	}
	expr.Value = src
}

// scriptCalls are the view functions whose result is a script for an on*
// attribute: live events and JS commands.
var scriptCalls = []string{"Send", "SendTo", "Submit", "SubmitTo", "Change", "ChangeTo", "JS"}

func isScriptCall(e ast.Expr) bool {
	for _, name := range scriptCalls {
		if isViewCall(e, name) {
			return true
		}
	}
	return false
}

func (c *component) attrLiteral(lit *ast.CompositeLit, full string, at parser.Expression, base int) (string, bool) {
	var static, parts []string
	sends := false
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return "", false
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			return "", false
		}
		name, _ := strconv.Unquote(key.Value)
		val := full[kv.Value.Pos()-1 : kv.Value.End()-1]
		offset := strconv.Itoa(int(kv.Value.Pos()) - 1 - base)
		if strings.HasPrefix(name, "on") && isScriptCall(kv.Value) {
			// templ drops a script value inside templ.Attributes: pass the
			// live event as the attribute's text instead.
			static = append(static, key.Value+": view.ScriptAttr("+val+")")
			sends = true
			continue
		}
		handler := strings.HasPrefix(name, "on") && jsgen.IsAction(kv.Value)
		if !handler && len(c.signalReads(kv.Value)) == 0 {
			static = append(static, key.Value+": "+val)
			continue
		}
		js, caps, err := c.compile(val, handler)
		if err != nil {
			c.failExpr(at, err, fmt.Sprintf("templ.Attributes %q: ", name))
			return "", false
		}
		kind := "b"
		if handler {
			kind = "h"
		}
		id := c.id(kind, at.Range.From, name+"@"+offset)
		c.twins[id] = js
		c.uses = true
		if handler {
			parts = append(parts, fmt.Sprintf("view.OnAttr(%q, %q, %s, %s)", name[2:], id, caps, val))
		} else {
			parts = append(parts, fmt.Sprintf("view.Bind(%q, %q, %s, %s)", name, id, caps, val))
		}
	}
	if len(parts) == 0 {
		if sends {
			return "templ.Attributes{" + strings.Join(static, ", ") + "}", true
		}
		return "", false
	}
	return "view.Attrs(templ.Attributes{" + strings.Join(static, ", ") + "}, " + strings.Join(parts, ", ") + ")", true
}
