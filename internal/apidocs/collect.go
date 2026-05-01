package apidocs

import (
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"go/types"
	"reflect"
	"runtime"
	"sort"
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
// the user's app root.
//
// Best-effort: type-check errors elsewhere in the tree (stale APIs,
// missing peer packages in a multi-repo setup) don't abort the scan;
// they're counted into Doc.LoadErrors so the renderer can show a
// "partial scan" banner.
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
	doc := &Doc{GoVersion: runtime.Version()}
	for _, p := range pkgs {
		doc.LoadErrors += len(p.Errors)
	}
	if len(pkgs) > 0 && pkgs[0].Module != nil {
		doc.Module = pkgs[0].Module.Path
	}

	st := newCollectState(pkgs, doc.Module)
	for _, pkg := range pkgs {
		// Skip framework codegen output: nexus emits zz_shadow_gen.go
		// files into .nexus/build/<deployment>/ for split builds. They
		// re-declare the same modules, which would double every entry
		// in the IR.
		if isShadowPkg(pkg) {
			continue
		}
		newPkgCollector(pkg, st).scan(doc)
	}
	doc.Entities = st.materializeEntities()
	return doc, nil
}

// isShadowPkg detects packages produced by `nexus build` overlay
// codegen, identified by the .nexus/build/ path segment in any of
// their source files.
func isShadowPkg(p *packages.Package) bool {
	for _, f := range p.GoFiles {
		if strings.Contains(f, "/.nexus/build/") {
			return true
		}
	}
	return false
}

// collectState is shared across every pkgCollector for one Collect
// run. It holds the global type index (so an entity defined in
// pkg/models can be resolved from a handler in modules/uaa) and the
// set of TypeNames referenced by handler signatures — those are
// the seed for the entity-materialization pass.
type collectState struct {
	rootModule string
	pkgs       []*packages.Package
	// typeIndex maps every type-name declared in any loaded package
	// to its source location, enabling entity resolution across
	// package boundaries.
	typeIndex map[*types.TypeName]*typeSite
	// funcsIndex maps every function/method declared in any loaded
	// package to its FuncDecl, so the entity pass can attach doc
	// comments and source positions to method-set entries.
	funcsIndex map[*types.Func]funcSite
	// referenced is the worklist seeded by signature(); the entity
	// pass expands it transitively before rendering.
	referenced map[*types.TypeName]bool
}

// funcSite carries the AST + package context for a function or method.
type funcSite struct {
	pkg  *packages.Package
	decl *ast.FuncDecl
}

// typeSite captures everything we need to render an Entity from a
// *types.TypeName: its declaration's AST (for the field doc comments
// we'd grab in a future iteration) and its package context.
type typeSite struct {
	pkg  *packages.Package
	spec *ast.TypeSpec
	doc  string
	pos  Pos
}

func newCollectState(pkgs []*packages.Package, rootModule string) *collectState {
	return &collectState{
		rootModule: rootModule,
		pkgs:       pkgs,
		typeIndex:  buildTypeIndex(pkgs),
		funcsIndex: buildFuncsIndex(pkgs),
		referenced: map[*types.TypeName]bool{},
	}
}

// buildFuncsIndex walks every loaded package's top-level FuncDecls
// (both free functions and methods) and indexes them by their
// *types.Func object. Used during entity materialization to attach
// doc comments to method-set entries.
func buildFuncsIndex(pkgs []*packages.Package) map[*types.Func]funcSite {
	out := map[*types.Func]funcSite{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if obj, ok := p.TypesInfo.Defs[fd.Name].(*types.Func); ok {
					out[obj] = funcSite{pkg: p, decl: fd}
				}
			}
		}
	}
	return out
}

// buildTypeIndex walks every loaded package's top-level type decls
// and records a typeSite per declared TypeName. Doc comments are
// pulled from the type spec when present, or the enclosing GenDecl
// for the `var (...)` style declaration that puts the doc one level
// up.
func buildTypeIndex(pkgs []*packages.Package) map[*types.TypeName]*typeSite {
	out := map[*types.TypeName]*typeSite{}
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			for _, d := range f.Decls {
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, sp := range gd.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					if !ok {
						continue
					}
					obj, _ := p.TypesInfo.Defs[ts.Name].(*types.TypeName)
					if obj == nil {
						continue
					}
					site := &typeSite{pkg: p, spec: ts, pos: posOf(p.Fset, ts.Pos())}
					switch {
					case ts.Doc != nil:
						site.doc = strings.TrimSpace(ts.Doc.Text())
					case gd.Doc != nil:
						site.doc = strings.TrimSpace(gd.Doc.Text())
					}
					out[obj] = site
				}
			}
		}
	}
	return out
}

// shouldDocument decides whether a TypeName earns an Entity entry —
// must live under the user's module path (so we don't try to render
// stdlib or third-party types) and must resolve in our index (so we
// have the source to work from).
func (s *collectState) shouldDocument(tn *types.TypeName) bool {
	if tn == nil || tn.Pkg() == nil {
		return false
	}
	if !strings.HasPrefix(tn.Pkg().Path(), s.rootModule) {
		return false
	}
	_, ok := s.typeIndex[tn]
	return ok
}

// note records a TypeName as referenced. Returns the slug if it
// will be documented, "" otherwise — the caller stores the slug on
// the IR field for cross-linking.
func (s *collectState) note(tn *types.TypeName) string {
	if !s.shouldDocument(tn) {
		return ""
	}
	s.referenced[tn] = true
	return entitySlug(tn)
}

// noteType walks a Type expression to find any *types.Named within
// (through pointers, slices, arrays, maps). Used at signature time
// so that `*[]models.Note` registers `models.Note` as referenced.
// Returns the slug for the first Named encountered (the most useful
// link target — the visible name in the rendered type string).
func (s *collectState) noteType(t types.Type) string {
	first := ""
	walkType(t, func(tn *types.TypeName) {
		slug := s.note(tn)
		if first == "" {
			first = slug
		}
	})
	return first
}

// materializeEntities expands the referenced set transitively into
// Entity records. As each entity's struct fields are walked, any
// in-module Named struct field types are pulled into the worklist —
// so referencing one type pulls its supporting types in too.
func (s *collectState) materializeEntities() []Entity {
	seen := map[*types.TypeName]bool{}
	out := []Entity{}
	queue := make([]*types.TypeName, 0, len(s.referenced))
	for tn := range s.referenced {
		queue = append(queue, tn)
	}
	for len(queue) > 0 {
		tn := queue[0]
		queue = queue[1:]
		if seen[tn] {
			continue
		}
		seen[tn] = true
		site, ok := s.typeIndex[tn]
		if !ok {
			continue
		}
		named, ok := tn.Type().(*types.Named)
		if !ok {
			continue
		}
		methods := s.collectMethods(named)
		st, ok := named.Underlying().(*types.Struct)
		if !ok {
			// Non-struct named types (enums, type aliases) — fields
			// stay empty but methods may still be present.
			out = append(out, Entity{
				Name:    tn.Name(),
				Pkg:     tn.Pkg().Path(),
				Slug:    entitySlug(tn),
				Doc:     site.doc,
				Pos:     site.pos,
				Methods: methods,
			})
			continue
		}
		ent := Entity{
			Name:    tn.Name(),
			Pkg:     tn.Pkg().Path(),
			Slug:    entitySlug(tn),
			Doc:     site.doc,
			Pos:     site.pos,
			Methods: methods,
		}
		for i := 0; i < st.NumFields(); i++ {
			f := st.Field(i)
			if !f.Exported() {
				continue
			}
			tags := parseTags(st.Tag(i))
			ef := EntityField{
				Name: f.Name(),
				Type: types.TypeString(f.Type(), nil),
				Tags: tags,
			}
			// Recurse: each in-module Named struct field type joins
			// the worklist so the entity graph closes.
			walkType(f.Type(), func(child *types.TypeName) {
				if !seen[child] && s.shouldDocument(child) {
					queue = append(queue, child)
				}
				if ef.TypeLink == "" {
					if slug := s.note(child); slug != "" {
						ef.TypeLink = slug
					}
				}
			})
			ent.Fields = append(ent.Fields, ef)
		}
		out = append(out, ent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pkg != out[j].Pkg {
			return out[i].Pkg < out[j].Pkg
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// collectMethods walks the method set of a Named type — both
// value-receiver and pointer-receiver methods, since *types.Named
// exposes value-receiver methods directly and we add pointer ones via
// types.NewPointer. Doc + position come from the FuncDecl in the
// global funcs index; methods declared in unloaded packages get name
// + signature only.
//
// Only exported methods are emitted. Methods inherited via struct
// embedding are captured by types.NewMethodSet — useful for service
// wrappers that embed *nexus.Service.
func (s *collectState) collectMethods(named *types.Named) []EntityMethod {
	seen := map[*types.Func]bool{}
	collect := func(t types.Type) []EntityMethod {
		mset := types.NewMethodSet(t)
		out := []EntityMethod{}
		for i := 0; i < mset.Len(); i++ {
			fn, ok := mset.At(i).Obj().(*types.Func)
			if !ok || !fn.Exported() || seen[fn] {
				continue
			}
			seen[fn] = true
			sig, ok := fn.Type().(*types.Signature)
			if !ok {
				continue
			}
			recv := ""
			if r := sig.Recv(); r != nil {
				if _, isPtr := r.Type().(*types.Pointer); isPtr {
					recv = "pointer"
				} else {
					recv = "value"
				}
			}
			em := EntityMethod{
				Name:      fn.Name(),
				Signature: signatureString(sig),
				Receiver:  recv,
			}
			if site, ok := s.funcsIndex[fn]; ok {
				if site.decl != nil && site.decl.Doc != nil {
					em.Doc = strings.TrimSpace(site.decl.Doc.Text())
				}
				em.Pos = posOf(site.pkg.Fset, fn.Pos())
			}
			out = append(out, em)
		}
		return out
	}
	// Value-receiver method set, then pointer-receiver — pointer set
	// is a superset, but the seen map keeps duplicates out and we
	// preserve receiver kind by querying the method's actual signature
	// receiver above.
	methods := collect(named)
	methods = append(methods, collect(types.NewPointer(named))...)
	sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
	return methods
}

// signatureString renders a *types.Signature without the receiver and
// without the function name — just `(params) (results)`. Matches how
// godoc shows method signatures, with package basenames instead of
// full import paths so the output reads like Go source.
func signatureString(sig *types.Signature) string {
	q := func(p *types.Package) string {
		if p == nil {
			return ""
		}
		return p.Name()
	}
	var b strings.Builder
	b.WriteByte('(')
	for i := 0; i < sig.Params().Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		p := sig.Params().At(i)
		if p.Name() != "" {
			b.WriteString(p.Name())
			b.WriteByte(' ')
		}
		b.WriteString(types.TypeString(p.Type(), q))
	}
	b.WriteByte(')')
	results := sig.Results()
	if results.Len() == 0 {
		return b.String()
	}
	b.WriteByte(' ')
	if results.Len() > 1 {
		b.WriteByte('(')
	}
	for i := 0; i < results.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(types.TypeString(results.At(i).Type(), q))
	}
	if results.Len() > 1 {
		b.WriteByte(')')
	}
	return b.String()
}

// walkType descends through composite type wrappers (pointer, slice,
// array, map) and calls fn for every named type at the leaves. Used
// for both link resolution at signature time and the recursive
// closure during entity materialization.
func walkType(t types.Type, fn func(*types.TypeName)) {
	if t == nil {
		return
	}
	switch v := t.(type) {
	case *types.Named:
		if v.Obj() != nil {
			fn(v.Obj())
		}
	case *types.Pointer:
		walkType(v.Elem(), fn)
	case *types.Slice:
		walkType(v.Elem(), fn)
	case *types.Array:
		walkType(v.Elem(), fn)
	case *types.Map:
		walkType(v.Key(), fn)
		walkType(v.Elem(), fn)
	}
}

// entitySlug builds the stable HTML anchor id for a TypeName: derived
// from its package path + name, with non-alphanumerics collapsed to
// dashes. Stable across runs so links survive page reloads.
func entitySlug(tn *types.TypeName) string {
	if tn == nil || tn.Pkg() == nil {
		return ""
	}
	raw := tn.Pkg().Path() + "-" + tn.Name()
	var b strings.Builder
	b.WriteString("entity-")
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// pkgCollector holds per-package state — chiefly the *types.Func →
// *ast.FuncDecl map we use to chase a constructor reference (an
// *ast.Ident inside an AsQuery call) back to its declaration so we
// can read its doc comment.
type pkgCollector struct {
	pkg   *packages.Package
	fset  *token.FileSet
	funcs map[*types.Func]*ast.FuncDecl
	state *collectState
}

func newPkgCollector(p *packages.Package, st *collectState) *pkgCollector {
	c := &pkgCollector{pkg: p, fset: p.Fset, funcs: map[*types.Func]*ast.FuncDecl{}, state: st}
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
		h.Deps, h.Args, h.Returns, h.ReturnsLink = c.signature(fn)
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
// type as the pretty-printed return + its entity slug if the type is
// in-module.
func (c *pkgCollector) signature(fn *types.Func) ([]Dep, *Args, string, string) {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil, nil, "", ""
	}
	var deps []Dep
	var args *Args
	params := sig.Params()
	q := qualifier(c.pkg)
	for i := 0; i < params.Len(); i++ {
		p := params.At(i)
		pt := p.Type()
		if a := c.paramsArg(pt); a != nil {
			args = a
			continue
		}
		if i == params.Len()-1 {
			if a := c.structArg(pt); a != nil {
				args = a
				continue
			}
		}
		dep := Dep{Name: p.Name(), Type: types.TypeString(pt, q)}
		dep.TypeLink = c.state.noteType(pt)
		deps = append(deps, dep)
	}
	var ret, retLink string
	for i := 0; i < sig.Results().Len(); i++ {
		r := sig.Results().At(i)
		if isError(r.Type()) {
			continue
		}
		ret = types.TypeString(r.Type(), q)
		retLink = c.state.noteType(r.Type())
		break
	}
	return deps, args, ret, retLink
}

// paramsArg returns the Args extracted from a nexus.Params[T] type.
// Returns nil for any other type.
func (c *pkgCollector) paramsArg(t types.Type) *Args {
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
	return c.structArg(targs.At(0))
}

// structArg flattens a struct or named-struct into Args. Empty
// `struct{}` (used by handlers that take no input) returns a non-nil
// Args with no fields, which is meaningful — renderers can show
// "no arguments" instead of omitting the section.
func (c *pkgCollector) structArg(t types.Type) *Args {
	a := &Args{}
	if n, ok := t.(*types.Named); ok && n.Obj() != nil {
		a.Type = n.Obj().Name()
		a.TypeLink = c.state.note(n.Obj())
	}
	st, ok := t.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	for i := 0; i < st.NumFields(); i++ {
		f := st.Field(i)
		if !f.Exported() {
			continue
		}
		tags := parseTags(st.Tag(i))
		fld := Field{
			Name:     f.Name(),
			Type:     types.TypeString(f.Type(), nil),
			TypeLink: c.state.noteType(f.Type()),
			Tags:     tags,
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
// framework package.
func (c *pkgCollector) isNexusCall(call *ast.CallExpr, name string) bool {
	return matchSelector(c.pkg.TypesInfo, call.Fun, name)
}

// isNexusCallIndexed matches the generic form `nexus.<name>[T](...)`.
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

	// Register the resource type as an entity if it lives in-module
	// — every generated handler returns or accepts T.
	tLink := c.state.noteType(tType)

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

	resourceArgs := c.structArg(tType)
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
			HTTP: &HTTP{Method: "GET", Path: restPath}, Returns: "[]" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "rest", Name: "Get" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Read by id.",
			HTTP: &HTTP{Method: "GET", Path: restPath + "/:id"}, Args: idArgs, Returns: "*" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "rest", Name: "Create" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Create from body.",
			HTTP: &HTTP{Method: "POST", Path: restPath}, Args: resourceArgs, Returns: "*" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "rest", Name: "Update" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Shallow merge by id.",
			HTTP: &HTTP{Method: "PATCH", Path: restPath + "/:id"}, Args: resourceArgs, Returns: "*" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "rest", Name: "Delete" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. Remove by id.",
			HTTP: &HTTP{Method: "DELETE", Path: restPath + "/:id"}, Args: idArgs, Returns: ""})
	}
	if enableGQL {
		add(Handler{Kind: "query", Name: "list" + tName + "s", Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL list " + plural + ".",
			Returns: "[]" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "query", Name: "get" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL fetch one.",
			Args: idArgs, Returns: "*" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "mutation", Name: "create" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL create.",
			Args: resourceArgs, Returns: "*" + tName, ReturnsLink: tLink})
		add(Handler{Kind: "mutation", Name: "update" + tName, Doc: "Generated by nexus.AsCRUD[" + tName + "]. GraphQL update.",
			Args: resourceArgs, Returns: "*" + tName, ReturnsLink: tLink})
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
	return posOf(c.fset, p)
}

// posOf is the package-level form, used by the type-index builder
// that doesn't have a pkgCollector around.
func posOf(fset *token.FileSet, p token.Pos) Pos {
	pos := fset.Position(p)
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