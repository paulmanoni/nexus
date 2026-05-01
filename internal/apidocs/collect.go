package apidocs

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

// nexusPkgPath is the import path the collector matches when deciding
// whether a `pkg.Foo(...)` call is one of our framework hooks. Kept as
// a constant so a future fork or rename only changes one line.
const nexusPkgPath = "github.com/paulmanoni/nexus"

// Collect loads the package(s) matching `pattern` from `dir` and
// produces an IR Doc by scanning for nexus framework calls.
//
// `pattern` follows go/packages conventions ("./...", ".", a package
// path). `dir` is the working directory the load runs from — usually
// the user's app root. Errors from the loader (missing module, type
// errors) come back wrapped; package-level type-check errors are
// surfaced via packages.PrintErrors and turned into a single err
// rather than silently producing partial IR.
func Collect(dir, pattern string) (*Doc, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports |
			packages.NeedDeps | packages.NeedModule,
		Dir: dir,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", pattern, err)
	}
	// Best-effort: type errors elsewhere in the tree (stale APIs,
	// missing peer packages in a multi-repo setup) shouldn't stop us
	// from documenting what did parse. Count them so the caller can
	// surface a one-line summary rather than scrolling a wall of
	// compiler output.
	doc := &Doc{GoVersion: runtime.Version()}
	for _, p := range pkgs {
		doc.LoadErrors += len(p.Errors)
	}
	if len(pkgs) > 0 && pkgs[0].Module != nil {
		doc.Module = pkgs[0].Module.Path
	}
	for _, pkg := range pkgs {
		// Skip framework codegen output: nexus emits zz_shadow_gen.go
		// files into .nexus/build/<deployment>/ for split builds. They
		// re-declare the same modules, which would double every entry
		// in the IR.
		if isShadowPkg(pkg) {
			continue
		}
		newPkgCollector(pkg).scan(doc)
	}
	return doc, nil
}

// isShadowPkg detects packages produced by `nexus build` overlay
// codegen, identified by the .nexus/build/ path segment in any of
// their source files. Filtering these out keeps the IR free of the
// duplicate registrations the build cache holds.
func isShadowPkg(p *packages.Package) bool {
	for _, f := range p.GoFiles {
		if strings.Contains(f, "/.nexus/build/") {
			return true
		}
	}
	return false
}

// pkgCollector holds per-package state — chiefly the *types.Func →
// *ast.FuncDecl map we use to chase a constructor reference (an
// *ast.Ident inside an AsQuery call) back to its declaration so we
// can read its doc comment.
type pkgCollector struct {
	pkg   *packages.Package
	fset  *token.FileSet
	funcs map[*types.Func]*ast.FuncDecl
}

func newPkgCollector(p *packages.Package) *pkgCollector {
	c := &pkgCollector{pkg: p, fset: p.Fset, funcs: map[*types.Func]*ast.FuncDecl{}}
	for _, f := range p.Syntax {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
				c.funcs[obj] = fd
			}
		}
	}
	return c
}

// scan walks every top-level decl in the package looking for var
// initializers that call nexus.Module(...) and bare As*() calls
// (rare but legal — a free-standing AsRest in main.go, for example).
func (c *pkgCollector) scan(out *Doc) {
	for _, f := range c.pkg.Syntax {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, val := range vs.Values {
					call, ok := val.(*ast.CallExpr)
					if !ok {
						continue
					}
					if c.isNexusCall(call, "Module") {
						mod := c.module(call)
						if gd.Doc != nil {
							mod.Doc = strings.TrimSpace(gd.Doc.Text())
						}
						out.Modules = append(out.Modules, mod)
						continue
					}
					for _, h := range c.maybeHandlers(call, "") {
						out.Loose = append(out.Loose, h)
					}
				}
			}
		}
	}
}

// module turns a `nexus.Module("name", opts...)` call into a Module IR.
// The first arg must be a string literal; computed names are skipped
// (we'd need const folding to handle them and v0 prefers obvious-fail).
//
// Two passes: first capture meta (RoutePath, Provides) so AsCRUD
// expansion can prefix generated REST paths regardless of source
// order; second emit handlers using the captured prefix.
func (c *pkgCollector) module(call *ast.CallExpr) Module {
	m := Module{Pos: c.posOf(call.Pos())}
	if len(call.Args) == 0 {
		return m
	}
	if name, ok := stringLit(call.Args[0]); ok {
		m.Name = name
	}
	rest := call.Args[1:]
	for _, arg := range rest {
		ce, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		switch {
		case c.isNexusCall(ce, "Path"), c.isNexusCall(ce, "RoutePrefix"):
			if len(ce.Args) > 0 {
				if s, ok := stringLit(ce.Args[0]); ok {
					m.RoutePath = s
				}
			}
		case c.isNexusCall(ce, "Provide"):
			for _, a := range ce.Args {
				m.Provides = append(m.Provides, c.exprStr(a))
			}
		}
	}
	for _, arg := range rest {
		ce, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}
		if c.isNexusCall(ce, "Path") || c.isNexusCall(ce, "RoutePrefix") || c.isNexusCall(ce, "Provide") {
			continue
		}
		m.Handlers = append(m.Handlers, c.maybeHandlers(ce, m.RoutePath)...)
	}
	return m
}

// maybeHandlers decides whether a CallExpr is one of the registration
// forms and returns the resulting Handler IR(s). One call usually
// produces one Handler; nexus.AsCRUD[T](...) produces up to ten
// (5 REST + 5 GraphQL ops). Returns nil for everything else.
//
// `prefix` is the enclosing module's RoutePath, applied to AsCRUD's
// generated REST paths so docs match the runtime mount.
func (c *pkgCollector) maybeHandlers(call *ast.CallExpr, prefix string) []Handler {
	switch {
	case c.isNexusCall(call, "AsQuery"):
		return []Handler{c.handler(call, "query", 0)}
	case c.isNexusCall(call, "AsMutation"):
		return []Handler{c.handler(call, "mutation", 0)}
	case c.isNexusCall(call, "AsRest"):
		if len(call.Args) < 3 {
			return nil
		}
		method, _ := stringLit(call.Args[0])
		path, _ := stringLit(call.Args[1])
		h := c.handler(call, "rest", 2)
		h.HTTP = &HTTP{Method: method, Path: path}
		return []Handler{h}
	case c.isNexusCall(call, "AsWS"):
		if len(call.Args) < 3 {
			return nil
		}
		path, _ := stringLit(call.Args[0])
		msg, _ := stringLit(call.Args[1])
		h := c.handler(call, "ws", 2)
		h.WS = &WS{Path: path, MessageType: msg}
		return []Handler{h}
	case c.isNexusCallIndexed(call, "AsCRUD"):
		return c.crud(call, prefix)
	}
	return nil
}

// handler builds the Handler IR for one registration. fnIdx is the
// index of the constructor argument (0 for AsQuery/AsMutation, 2 for
// AsRest/AsWS); everything after it is captured as Options verbatim.
func (c *pkgCollector) handler(call *ast.CallExpr, kind string, fnIdx int) Handler {
	h := Handler{Kind: kind, Pos: c.posOf(call.Pos())}
	if fnIdx >= len(call.Args) {
		return h
	}
	fnExpr := call.Args[fnIdx]
	h.Func = c.exprStr(fnExpr)
	h.Name = opName(h.Func)
	if fn := c.resolveFunc(fnExpr); fn != nil {
		if decl := c.funcs[fn]; decl != nil && decl.Doc != nil {
			h.Doc = strings.TrimSpace(decl.Doc.Text())
		}
		h.Deps, h.Args, h.Returns = c.signature(fn)
	}
	for _, a := range call.Args[fnIdx+1:] {
		h.Options = append(h.Options, Opt{Expr: c.exprStr(a)})
	}
	return h
}

// resolveFunc walks an Ident or SelectorExpr argument back to its
// declared *types.Func via TypesInfo.Uses. Returns nil for func
// literals or anything else exotic — those simply won't get doc
// comments, which is acceptable for v0.
func (c *pkgCollector) resolveFunc(e ast.Expr) *types.Func {
	var ident *ast.Ident
	switch v := e.(type) {
	case *ast.Ident:
		ident = v
	case *ast.SelectorExpr:
		ident = v.Sel
	default:
		return nil
	}
	if obj, ok := c.pkg.TypesInfo.Uses[ident]; ok {
		if fn, ok := obj.(*types.Func); ok {
			return fn
		}
	}
	return nil
}

// signature splits a constructor's params into Deps (services / DBs /
// caches injected by fx) and Args (the Params[T] or trailing struct
// holding user-supplied input). Returns the first non-error result
// type as the pretty-printed return.
func (c *pkgCollector) signature(fn *types.Func) ([]Dep, *Args, string) {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil, nil, ""
	}
	var deps []Dep
	var args *Args
	params := sig.Params()
	q := qualifier(c.pkg)
	for i := 0; i < params.Len(); i++ {
		p := params.At(i)
		pt := p.Type()
		if a := paramsArg(pt); a != nil {
			args = a
			continue
		}
		// Last param may be a trailing args struct without Params[T].
		if i == params.Len()-1 {
			if a := structArg(pt); a != nil {
				args = a
				continue
			}
		}
		deps = append(deps, Dep{Name: p.Name(), Type: types.TypeString(pt, q)})
	}
	var ret string
	for i := 0; i < sig.Results().Len(); i++ {
		r := sig.Results().At(i)
		if isError(r.Type()) {
			continue
		}
		ret = types.TypeString(r.Type(), q)
		break
	}
	return deps, args, ret
}

// paramsArg returns the Args extracted from a nexus.Params[T] type.
// Returns nil for any other type. Match is by package path + name so
// a user with a local `Params` type can't accidentally collide.
func paramsArg(t types.Type) *Args {
	n, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	obj := n.Obj()
	if obj == nil || obj.Pkg() == nil {
		return nil
	}
	if obj.Name() != "Params" || obj.Pkg().Path() != nexusPkgPath {
		return nil
	}
	targs := n.TypeArgs()
	if targs == nil || targs.Len() == 0 {
		return nil
	}
	return structArg(targs.At(0))
}

// structArg flattens a struct or named-struct into Args. Empty
// `struct{}` (used by handlers that take no input) returns a non-nil
// Args with no fields, which is meaningful — renderers can show
// "no arguments" instead of omitting the section.
func structArg(t types.Type) *Args {
	name := ""
	if n, ok := t.(*types.Named); ok && n.Obj() != nil {
		name = n.Obj().Name()
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	a := &Args{Type: name}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		tags := parseTags(st.Tag(i))
		fld := Field{
			Name: f.Name(),
			Type: types.TypeString(f.Type(), nil),
			Tags: tags,
		}
		if _, isPtr := f.Type().(*types.Pointer); isPtr {
			fld.Optional = true
		} else if g, ok := tags["graphql"]; ok && !strings.Contains(g, "required") {
			fld.Optional = true
		}
		a.Fields = append(a.Fields, fld)
	}
	return a
}

// parseTags extracts the handful of struct tags the framework cares
// about. Unknown tags are dropped so the IR doesn't carry noise from
// gorm / mapstructure / etc. — those belong to other concerns.
func parseTags(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	st := reflect.StructTag(raw)
	out := map[string]string{}
	for _, k := range []string{"graphql", "json", "validate", "path", "form", "query", "header"} {
		if v, ok := st.Lookup(k); ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// qualifier returns a types.Qualifier that omits the package name for
// types declared in the user's own package and uses the package name
// (not the full path) for everything else, matching how the source
// reads.
func qualifier(pkg *packages.Package) types.Qualifier {
	return func(p *types.Package) string {
		if p == pkg.Types {
			return ""
		}
		return p.Name()
	}
}

// isError matches the universe `error` type — its Obj().Pkg() is nil
// because it's predeclared, which is the whole identification trick.
func isError(t types.Type) bool {
	n, ok := t.(*types.Named)
	if !ok {
		return false
	}
	return n.Obj() != nil && n.Obj().Name() == "error" && n.Obj().Pkg() == nil
}

// isNexusCall reports whether call is `nexus.<name>(...)` from the
// framework package. Resolves the receiver through TypesInfo so an
// aliased import (`nexus "github.com/paulmanoni/nexus"`) still works.
func (c *pkgCollector) isNexusCall(call *ast.CallExpr, name string) bool {
	return matchSelector(c.pkg.TypesInfo, call.Fun, name)
}

// isNexusCallIndexed matches the generic form `nexus.<name>[T](...)`
// (single or multiple type arguments). The CallExpr.Fun is an
// *ast.IndexExpr or *ast.IndexListExpr wrapping the SelectorExpr.
func (c *pkgCollector) isNexusCallIndexed(call *ast.CallExpr, name string) bool {
	switch fn := call.Fun.(type) {
	case *ast.IndexExpr:
		return matchSelector(c.pkg.TypesInfo, fn.X, name)
	case *ast.IndexListExpr:
		return matchSelector(c.pkg.TypesInfo, fn.X, name)
	}
	return false
}

// matchSelector is the shared SelectorExpr check used by the two
// isNexusCall* helpers — extracted so generic and non-generic call
// shapes share the receiver resolution rule.
func matchSelector(info *types.Info, fun ast.Expr, name string) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	pn, ok := info.Uses[pkgIdent].(*types.PkgName)
	if !ok {
		return false
	}
	return pn.Imported().Path() == nexusPkgPath
}

// crud expands a `nexus.AsCRUD[T](resolver, opts...)` call into the
// list of synthetic Handlers the framework would mount at runtime.
//
// Defaults: REST on (5 ops). GraphQL is opt-in via WithGraphQL();
// WithoutREST() suppresses the REST half. Path stem is the lowercased,
// naive-pluralized type name ("Note" → "/notes"); a non-empty module
// prefix is joined in front so docs match the deployed URLs.
func (c *pkgCollector) crud(call *ast.CallExpr, prefix string) []Handler {
	idx, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return nil
	}
	tArg := idx.Index
	tName := c.exprStr(tArg)
	tType := c.pkg.TypesInfo.TypeOf(tArg)

	enableGQL, disableREST := false, false
	options := []Opt{}
	for _, a := range call.Args[1:] {
		expr := c.exprStr(a)
		options = append(options, Opt{Expr: expr})
		switch {
		case strings.HasPrefix(expr, "nexus.WithGraphQL"):
			enableGQL = true
		case strings.HasPrefix(expr, "nexus.WithoutREST"):
			disableREST = true
		}
	}

	pos := c.posOf(call.Pos())
	stem := "/" + strings.ToLower(tName) + "s"
	restPath := strings.TrimRight(prefix, "/") + stem
	plural := strings.ToLower(tName) + "s"

	resourceArgs := structArg(tType)
	idArgs := &Args{Fields: []Field{{Name: "ID", Type: "string", Tags: map[string]string{"path": "id"}}}}

	out := []Handler{}
	add := func(h Handler) {
		h.Pos = pos
		h.Func = "AsCRUD[" + tName + "]"
		h.Options = options
		out = append(out, h)
	}

	if !disableREST {
		add(Handler{Kind: "rest", Name: "List" + tName + "s", Doc: "Generated by nexus.AsCRUD[" + tName + "]. List with ?limit / ?offset / ?sort.",
			HTTP: &HTTP{Method: "GET", Path: restPath}, Returns: "[]" + tName})
		add(Handler{Kind: "rest", Name: "Get" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Read by id.",
			HTTP: &HTTP{Method: "GET", Path: restPath + "/:id"}, Args: idArgs, Returns: "*" + tName})
		add(Handler{Kind: "rest", Name: "Create" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Create from body.",
			HTTP: &HTTP{Method: "POST", Path: restPath}, Args: resourceArgs, Returns: "*" + tName})
		add(Handler{Kind: "rest", Name: "Update" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Shallow merge by id.",
			HTTP: &HTTP{Method: "PATCH", Path: restPath + "/:id"}, Args: resourceArgs, Returns: "*" + tName})
		add(Handler{Kind: "rest", Name: "Delete" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Remove by id.",
			HTTP: &HTTP{Method: "DELETE", Path: restPath + "/:id"}, Args: idArgs, Returns: ""})
	}
	if enableGQL {
		add(Handler{Kind: "query", Name: "list" + tName + "s", Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL list " + plural + ".",
			Returns: "[]" + tName})
		add(Handler{Kind: "query", Name: "get" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL fetch one.",
			Args: idArgs, Returns: "*" + tName})
		add(Handler{Kind: "mutation", Name: "create" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL create.",
			Args: resourceArgs, Returns: "*" + tName})
		add(Handler{Kind: "mutation", Name: "update" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL update.",
			Args: resourceArgs, Returns: "*" + tName})
		add(Handler{Kind: "mutation", Name: "delete" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL delete; returns Boolean.",
			Args: idArgs, Returns: "bool"})
	}
	return out
}

// exprStr renders an AST expression back to source. Used to keep
// option arguments verbatim (`auth.Required()`, `createRateLimit`)
// rather than re-implementing pretty-printing for each case.
func (c *pkgCollector) exprStr(e ast.Expr) string {
	var sb strings.Builder
	_ = printer.Fprint(&sb, c.fset, e)
	return sb.String()
}

func (c *pkgCollector) posOf(p token.Pos) Pos {
	pos := c.fset.Position(p)
	return Pos{File: pos.Filename, Line: pos.Line}
}

func stringLit(e ast.Expr) (string, bool) {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// opName turns a constructor reference like "NewCreateAdvert" into the
// op name "CreateAdvert". Mirrors the runtime's auto-mount naming so
// docs labels match what shows up in the dashboard / GraphQL schema.
func opName(funcRef string) string {
	name := funcRef
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimPrefix(name, "New")
}