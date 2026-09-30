// Package jsgen compiles the Go expressions of a reactive templ attribute to
// JavaScript. The Go stays the source of truth — the compiler type-checks
// it and the server evaluates it — while its JavaScript twin re-runs in the
// browser when a signal it reads changes.
//
// Only a vocabulary with the same meaning in both languages compiles;
// anything else is an error naming the construct, never a silent
// approximation.
package jsgen

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// Error is a construct outside the vocabulary, at an offset into the source.
type Error struct {
	Offset int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func errAt(n ast.Node, format string, args ...any) error {
	return &Error{Offset: int(n.Pos()) - 1, Msg: fmt.Sprintf(format, args...)}
}

// packages whose functions the vocabulary includes.
var packages = map[string]bool{"strings": true, "strconv": true, "view": true}

var builtins = map[string]bool{"true": true, "false": true, "nil": true, "len": true}

// Paths are field paths that hold signals — "s.Query" for the Query field of
// a state struct s. A path is captured and compiled as one value, so the
// browser never depends on a struct's field names.
type Paths map[string]bool

// SelectorPath returns "a.b.c" for a chain of identifiers, or "".
func SelectorPath(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x := SelectorPath(e.X); x != "" {
			return x + "." + e.Sel.Name
		}
	}
	return ""
}

// Captures lists the identifiers expr reads from its surroundings — the
// values the browser needs a snapshot of — sorted.
func Captures(expr string) ([]string, error) { return CapturesIn(expr, nil) }

// CapturesIn is Captures with signal paths.
func CapturesIn(expr string, paths Paths) ([]string, error) {
	e, err := parser.ParseExpr(expr)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var walk func(n ast.Node, locals map[string]bool)
	walk = func(n ast.Node, locals map[string]bool) {
		ast.Inspect(n, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncLit:
				inner := copyLocals(locals)
				for _, f := range n.Type.Params.List {
					for _, name := range f.Names {
						inner[name.Name] = true
					}
				}
				declared(n.Body, inner)
				walk(n.Body, inner)
				return false
			case *ast.SelectorExpr:
				if p := SelectorPath(n); paths[p] && !locals[strings.SplitN(p, ".", 2)[0]] {
					seen[p] = true
					return false
				}
				walk(n.X, locals)
				return false
			case *ast.KeyValueExpr:
				walk(n.Value, locals)
				return false
			case *ast.Ident:
				if !locals[n.Name] && !packages[n.Name] && !builtins[n.Name] {
					seen[n.Name] = true
				}
			}
			return true
		})
	}
	walk(e, map[string]bool{})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// declared adds the names a function body defines with := to locals.
func declared(body *ast.BlockStmt, locals map[string]bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && as.Tok == token.DEFINE {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					locals[id.Name] = true
				}
			}
		}
		_, isFunc := n.(*ast.FuncLit)
		return !isFunc
	})
}

func copyLocals(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Bind compiles a bound attribute's expression to `(c) => <js>`, where c
// holds the captured values.
func Bind(expr string) (string, error) { return BindIn(expr, nil) }

// BindIn is Bind with signal paths.
func BindIn(expr string, paths Paths) (string, error) {
	e, err := parser.ParseExpr(expr)
	if err != nil {
		return "", err
	}
	caps, err := CapturesIn(expr, paths)
	if err != nil {
		return "", err
	}
	t := newTranslator(caps)
	js, err := t.expr(e)
	if err != nil {
		return "", err
	}
	return "(c) => (" + js + ")", nil
}

// Handler compiles an on* attribute's action to `(c, e) => { … }`: a
// signal's Set (count.Set(count.Get() + 1)) or view.Do(func(e view.Event)
// { … }) for one that reads the event or takes several steps.
func Handler(expr string) (string, error) { return HandlerIn(expr, nil) }

// HandlerIn is Handler with signal paths.
func HandlerIn(expr string, paths Paths) (string, error) {
	e, err := parser.ParseExpr(expr)
	if err != nil {
		return "", err
	}
	caps, err := CapturesIn(expr, paths)
	if err != nil {
		return "", err
	}
	t := newTranslator(caps)
	if IsAction(e) && !isDo(e) {
		js, err := t.expr(e)
		if err != nil {
			return "", err
		}
		return "(c, e) => { " + js + "; }", nil
	}
	fn, err := handlerFunc(e)
	if err != nil {
		return "", err
	}
	params := fn.Type.Params.List
	event := "_e"
	if len(params) > 1 || (len(params) == 1 && len(params[0].Names) > 1) {
		return "", errAt(fn, "a handler takes one parameter, the event")
	}
	if len(params) == 1 && len(params[0].Names) == 1 && params[0].Names[0].Name != "_" {
		event = params[0].Names[0].Name
		t.locals[event] = true
		t.events[event] = true
	}
	body, err := t.block(fn.Body)
	if err != nil {
		return "", err
	}
	return "(c, " + event + ") => " + body, nil
}

// actionMethods are the signal methods that describe an event's effect.
var actionMethods = map[string]bool{"Set": true}

// IsAction reports whether e is an action an on* attribute compiles: a
// signal action method call or view.Do(…).
func IsAction(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if isIdent(sel.X, "view") {
		return sel.Sel.Name == "Do"
	}
	return actionMethods[sel.Sel.Name]
}

func isDo(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, "view") && sel.Sel.Name == "Do"
}

func handlerFunc(e ast.Expr) (*ast.FuncLit, error) {
	if call, ok := e.(*ast.CallExpr); ok {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "view") && sel.Sel.Name == "Do" && len(call.Args) == 1 {
			e = call.Args[0]
		}
	}
	fn, ok := e.(*ast.FuncLit)
	if !ok {
		return nil, errAt(e, "an action is a signal's Set, or view.Do(func(e view.Event) { … })")
	}
	return fn, nil
}

type translator struct {
	caps   map[string]bool
	locals map[string]bool
	events map[string]bool // parameters holding the DOM event
}

func newTranslator(caps []string) *translator {
	t := &translator{caps: map[string]bool{}, locals: map[string]bool{}, events: map[string]bool{}}
	for _, c := range caps {
		t.caps[c] = true
	}
	return t
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

var binaryOps = map[token.Token]string{
	token.ADD: "+", token.SUB: "-", token.MUL: "*",
	token.EQL: "===", token.NEQ: "!==",
	token.LSS: "<", token.LEQ: "<=", token.GTR: ">", token.GEQ: ">=",
	token.LAND: "&&", token.LOR: "||",
}

func (t *translator) expr(e ast.Expr) (string, error) {
	switch e := e.(type) {
	case *ast.BasicLit:
		return t.literal(e)
	case *ast.Ident:
		switch {
		case e.Name == "true" || e.Name == "false":
			return e.Name, nil
		case e.Name == "nil":
			return "null", nil
		case t.locals[e.Name]:
			return e.Name, nil
		case t.caps[e.Name]:
			return "c." + e.Name, nil
		}
		return "", errAt(e, "%s cannot be used here", e.Name)
	case *ast.ParenExpr:
		x, err := t.expr(e.X)
		if err != nil {
			return "", err
		}
		return "(" + x + ")", nil
	case *ast.UnaryExpr:
		if e.Op != token.NOT && e.Op != token.SUB && e.Op != token.ADD {
			return "", errAt(e, "the %s operator is not supported in a browser expression", e.Op)
		}
		x, err := t.expr(e.X)
		if err != nil {
			return "", err
		}
		return e.Op.String() + "(" + x + ")", nil
	case *ast.BinaryExpr:
		op, ok := binaryOps[e.Op]
		if !ok {
			return "", errAt(e, "the %s operator is not supported in a browser expression yet", e.Op)
		}
		x, err := t.expr(e.X)
		if err != nil {
			return "", err
		}
		y, err := t.expr(e.Y)
		if err != nil {
			return "", err
		}
		return "(" + x + " " + op + " " + y + ")", nil
	case *ast.SelectorExpr:
		return t.selector(e)
	case *ast.CallExpr:
		return t.call(e)
	}
	return "", errAt(e, "%s is not supported in a browser expression", describe(e))
}

func (t *translator) literal(e *ast.BasicLit) (string, error) {
	switch e.Kind {
	case token.INT, token.FLOAT:
		v := strings.ReplaceAll(e.Value, "_", "")
		if strings.ContainsAny(v, "xXoObB") && e.Kind == token.INT {
			n, err := strconv.ParseInt(v, 0, 64)
			if err != nil {
				return "", errAt(e, "%s: %v", e.Value, err)
			}
			v = strconv.FormatInt(n, 10)
		}
		return v, nil
	case token.STRING:
		s, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", errAt(e, "%s: %v", e.Value, err)
		}
		b, _ := json.Marshal(s)
		return string(b), nil
	}
	return "", errAt(e, "%s literals are not supported in a browser expression", e.Kind)
}

// eventFields maps view.Event and view.EventTarget fields to the DOM's.
var eventFields = map[string]string{"Target": "target", "Value": "value", "Checked": "checked", "Key": "key"}

func (t *translator) selector(e *ast.SelectorExpr) (string, error) {
	if p := SelectorPath(e); t.caps[p] {
		b, _ := json.Marshal(p)
		return "c[" + string(b) + "]", nil
	}
	if t.onEvent(e.X) {
		js, ok := eventFields[e.Sel.Name]
		if !ok {
			return "", errAt(e.Sel, "view.Event has no browser field %s", e.Sel.Name)
		}
		x, err := t.expr(e.X)
		if err != nil {
			return "", err
		}
		return x + "." + js, nil
	}
	return "", errAt(e, "field access is supported on the event only, not %s", describe(e))
}

// onEvent reports whether e is an event parameter or a field path from one.
func (t *translator) onEvent(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return t.events[e.Name]
	case *ast.SelectorExpr:
		return t.onEvent(e.X)
	}
	return false
}

var funcs = map[string]string{
	"strings.TrimSpace": "__nx.strings.trimSpace",
	"strings.ToUpper":   "__nx.strings.toUpper",
	"strings.ToLower":   "__nx.strings.toLower",
	"strings.Contains":  "__nx.strings.contains",
	"strings.HasPrefix": "__nx.strings.hasPrefix",
	"strings.HasSuffix": "__nx.strings.hasSuffix",
	"strconv.Itoa":      "__nx.strconv.itoa",
}

var signalMethods = map[string]int{"Get": 0, "Set": 1}

func (t *translator) call(e *ast.CallExpr) (string, error) {
	if e.Ellipsis.IsValid() {
		return "", errAt(e, "variadic calls are not supported in a browser expression")
	}
	args := make([]string, len(e.Args))
	for i, a := range e.Args {
		js, err := t.expr(a)
		if err != nil {
			return "", err
		}
		args[i] = js
	}
	switch fn := e.Fun.(type) {
	case *ast.Ident:
		if fn.Name == "len" && len(args) == 1 {
			return "__nx.len(" + args[0] + ")", nil
		}
		return "", errAt(fn, "%s cannot be called in a browser expression", fn.Name)
	case *ast.SelectorExpr:
		if pkg, ok := fn.X.(*ast.Ident); ok && packages[pkg.Name] && !t.caps[pkg.Name] && !t.locals[pkg.Name] {
			name := pkg.Name + "." + fn.Sel.Name
			js, ok := funcs[name]
			if !ok {
				return "", errAt(fn, "%s is not available in a browser expression", name)
			}
			return js + "(" + strings.Join(args, ", ") + ")", nil
		}
		want, ok := signalMethods[fn.Sel.Name]
		if !ok {
			if x, isIdent := fn.X.(*ast.Ident); isIdent {
				return "", errAt(fn, "%s.%s is not available in a browser expression — it supports a signal's Get and Set, and len plus some strings and strconv functions", x.Name, fn.Sel.Name)
			}
			return "", errAt(fn.Sel, "%s is not a method the browser supports (a signal has Get and Set)", fn.Sel.Name)
		}
		if len(args) != want {
			return "", errAt(e, "%s takes %d argument(s)", fn.Sel.Name, want)
		}
		recv, err := t.expr(fn.X)
		if err != nil {
			return "", err
		}
		return recv + "." + strings.ToLower(fn.Sel.Name) + "(" + strings.Join(args, ", ") + ")", nil
	}
	return "", errAt(e, "%s cannot be called in a browser expression", describe(e.Fun))
}

func (t *translator) block(b *ast.BlockStmt) (string, error) {
	var sb strings.Builder
	sb.WriteString("{ ")
	for _, s := range b.List {
		js, err := t.stmt(s)
		if err != nil {
			return "", err
		}
		sb.WriteString(js + " ")
	}
	sb.WriteString("}")
	return sb.String(), nil
}

func (t *translator) stmt(s ast.Stmt) (string, error) {
	switch s := s.(type) {
	case *ast.ExprStmt:
		js, err := t.expr(s.X)
		if err != nil {
			return "", err
		}
		return js + ";", nil
	case *ast.AssignStmt:
		if len(s.Lhs) != 1 || len(s.Rhs) != 1 {
			return "", errAt(s, "assign one value at a time in a browser handler")
		}
		id, ok := s.Lhs[0].(*ast.Ident)
		if !ok {
			return "", errAt(s.Lhs[0], "only local variables can be assigned in a browser handler — use Set on a signal")
		}
		rhs, err := t.expr(s.Rhs[0])
		if err != nil {
			return "", err
		}
		switch s.Tok {
		case token.DEFINE:
			t.locals[id.Name] = true
			return "let " + id.Name + " = " + rhs + ";", nil
		case token.ASSIGN:
			if !t.locals[id.Name] {
				return "", errAt(id, "%s is not a local variable — use Set on a signal", id.Name)
			}
			return id.Name + " = " + rhs + ";", nil
		}
		return "", errAt(s, "the %s assignment is not supported in a browser handler", s.Tok)
	case *ast.IfStmt:
		if s.Init != nil {
			return "", errAt(s.Init, "an if statement with an init clause is not supported in a browser handler")
		}
		cond, err := t.expr(s.Cond)
		if err != nil {
			return "", err
		}
		then, err := t.block(s.Body)
		if err != nil {
			return "", err
		}
		out := "if (" + cond + ") " + then
		if s.Else != nil {
			els, err := t.stmt(s.Else)
			if err != nil {
				return "", err
			}
			out += " else " + els
		}
		return out, nil
	case *ast.BlockStmt:
		return t.block(s)
	case *ast.ReturnStmt:
		if len(s.Results) > 0 {
			return "", errAt(s, "a handler returns nothing")
		}
		return "return;", nil
	}
	return "", errAt(s, "%s is not supported in a browser handler", describe(s))
}

func describe(n ast.Node) string {
	name := fmt.Sprintf("%T", n)
	name = strings.TrimPrefix(name, "*ast.")
	return "a " + name
}
