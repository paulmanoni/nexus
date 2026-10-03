package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// migrateSymbol is one exported name that moved or was renamed between v1
// and v2. Packages are written relative to the nexus module ("" is the root
// package, "config", "extension/auth", …), so the same row matches a file
// whether its imports are still on v1 or already on /v2.
//
// A row with a Todo instead of a New name has no mechanical replacement:
// each use gets a `// TODO(nexus v2): …` comment above its statement, and a
// use that is an element of an argument list or a composite literal (a Boot
// option, say) is dropped from it so the rest of the call still compiles.
type migrateSymbol struct {
	From, Old string // v1 package and name
	To, New   string // v2 package and name
	Todo      string // set instead of To/New for a manual migration
}

// migrateV2Symbols is the v1 → v2 symbol table. A later move (the dev
// package, the bus, slog) is one row here.
var migrateV2Symbols = []migrateSymbol{
	// Config moved out of the root (docs/design/v2.md §3).
	{From: "", Old: "Config", To: "config", New: "Runtime"},
	{From: "", Old: "ServerConfig", To: "config", New: "Server"},
	{From: "", Old: "WebSocketConfig", To: "config", New: "WebSocket"},
	{From: "", Old: "DashboardConfig", To: "config", New: "Dashboard"},
	{From: "", Old: "DevReloadConfig", To: "config", New: "DevReload"},
	{From: "", Old: "GraphQLConfig", To: "config", New: "GraphQL"},
	{From: "", Old: "MiddlewareConfig", To: "config", New: "Middleware"},
	{From: "", Old: "SecurityConfig", To: "config", New: "Security"},
	{From: "", Old: "CORSConfig", To: "config", New: "CORS"},
	{From: "", Old: "StoreConfig", To: "config", New: "Stores"},
	{From: "", Old: "Listener", To: "config", New: "Listener"},
	{From: "", Old: "ListenerScope", To: "config", New: "ListenerScope"},
	{From: "", Old: "ScopePublic", To: "config", New: "ScopePublic"},
	{From: "", Old: "ScopeInternal", To: "config", New: "ScopeInternal"},
	{From: "", Old: "ScopeAdmin", To: "config", New: "ScopeAdmin"},
	{From: "", Old: "DefaultConfigPath", To: "config", New: "DefaultPath"},
	{From: "", Old: "LoadConfig", To: "config", New: "Load"},
	{From: "", Old: "MustLoadConfig", To: "config", New: "MustLoad"},
	{From: "", Old: "Get", To: "config", New: "Get"},
	{From: "", Old: "MustGet", To: "config", New: "MustGet"},
	{From: "", Old: "HasConfig", To: "config", New: "Has"},
	{From: "", Old: "OnConfigChange", To: "config", New: "OnChange"},
	{From: "", Old: "BindConfig", To: "config", New: "Bind"},
	{From: "", Old: "ConfigVersion", To: "config", New: "Version"},
	{From: "", Old: "InstallConfigStore", To: "config", New: "InstallStore"},
	{From: "", Old: "UpdateConfigStore", To: "config", New: "UpdateStore"},
	{From: "", Old: "ClearConfigStoreForTest", To: "config", New: "ResetForTest"},
	{From: "", Old: "ConfigError", To: "config", New: "Error"},
	{From: "", Old: "LintRuntimeFile", To: "config", New: "LintFile"},
	{From: "", Old: "EnvVars", To: "config", New: "EnvVars"},
	{From: "", Old: "SkippedEnvVar", To: "config", New: "SkippedEnvVar"},
	{From: "", Old: "EnvVarsSkippingUnset", To: "config", New: "EnvVarsSkippingUnset"},
	{From: "", Old: "DotenvDefaultPath", To: "config", New: "DotenvDefaultPath"},
	{From: "", Old: "DatabaseSpec", To: "config", New: "DatabaseSpec"},
	{From: "", Old: "DatabaseSpecFor", To: "config", New: "DatabaseSpecFor"},
	{From: "", Old: "Cache", To: "resource", New: "Cache"},
	{From: "", Old: "MustLoadDotenv",
		Todo: "nexus.MustLoadDotenv() is gone; nexus.toml loads .env itself — list a required file as [runtime] dotenv = [\"!.env\"]"},
	{From: "", Old: "LoadDotenvIfPresent",
		Todo: "nexus.LoadDotenvIfPresent() is gone; nexus.toml loads .env beside it by default ([runtime] dotenv)"},

	// The dev package (docs/design/v2.md §3).
	{From: "", Old: "PreserveDev", To: "dev", New: "Preserve"},
	{From: "", Old: "PreserveDevJSON", To: "dev", New: "PreserveJSON"},
	{From: "", Old: "DevStateDir", To: "dev", New: "StateDir"},
	{From: "", Old: "DevState", To: "dev", New: "State"},
	{From: "", Old: "IsDev", To: "dev", New: "Enabled"},
	{From: "", Old: "NexusDevEnv", To: "dev", New: "Env"},
	{From: "", Old: "NexusDevRootEnv", To: "dev", New: "RootEnv"},

	// The notify package.
	{From: "", Old: "Notifier", To: "notify", New: "Notifier"},
	{From: "", Old: "NewNotifier", To: "notify", New: "New"},
	{From: "", Old: "Bus", To: "notify", New: "Bus"},

	// 1:1 replacements (docs/design/v2.md §9).
	{From: "", Old: "UseVolume", To: "", New: "DeclareVolume"},
	{From: "", Old: "ServeFrontend", To: "", New: "Frontend"},
	{From: "", Old: "AsRestHandler", To: "", New: "AsRest"},
	{From: "", Old: "ClientIPFromCtx", To: "", New: "ClientIP"},
	{From: "extension/ratelimit", Old: "ClientIPFromCtx", To: "", New: "ClientIP"},
	{From: "", Old: "WithClientIP",
		Todo: "nexus.WithClientIP is gone; the framework puts the caller's address on every request — read it with nexus.ClientIP(ctx)"},
	{From: "extension/ratelimit", Old: "WithClientIP",
		Todo: "ratelimit.WithClientIP is gone; the framework puts the caller's address on every request — read it with nexus.ClientIP(ctx)"},
	{From: "extension/auth", Old: "Describe", To: "extension/auth", New: "InspectExtractor"},

	// GraphQL behind a seam (docs/design/v2.md §5): graph and transport/gql
	// are internal; middleware and resolve info are gql types.
	{From: "graph", Old: "FieldMiddleware", To: "gql", New: "Middleware"},
	{From: "graph", Old: "FieldResolveFn", To: "gql", New: "Resolver"},
	{From: "graph", Old: "ResolveParams", To: "gql", New: "Field"},
	{From: "graph", Old: "GetRootInfo",
		Todo: "graph.GetRootInfo is gone; put the user on the context (Service.Auth's returned ctx, or extension/auth's auth.User[T])"},
	{From: "graph", Old: "GetRootString",
		Todo: "graph.GetRootString is gone; put the value on the context instead"},
	{From: "graph", Old: "NewResolver",
		Todo: "graph.NewResolver is internal; register the resolver as a handler with nexus.AsQuery / nexus.AsMutation"},
	{From: "graph", Old: "NewSubscription",
		Todo: "graph.NewSubscription is internal; push updates with nexus.AsWS"},
	{From: "graph", Old: "Validator",
		Todo: "graph validators are gone; use validate:\"…\" tags, and return nexus.Invalid().Field(…) from the handler for other checks"},
	{From: "graph", Old: "Required",
		Todo: "graph.Required is gone; tag the field validate:\"required\""},
	{From: "graph", Old: "StringLength",
		Todo: "graph.StringLength is gone; tag the field validate:\"len=min|max\""},
	{From: "graph", Old: "IntRange",
		Todo: "graph.IntRange is gone; tag the field validate:\"int=min|max\""},
	{From: "graph", Old: "OneOf",
		Todo: "graph.OneOf is gone; tag the field validate:\"oneof=a|b|c\""},
	{From: "graph", Old: "StringMatch",
		Todo: "graph.StringMatch is gone; check the value in the handler and return nexus.Invalid().Field(…)"},
	{From: "graph", Old: "Custom",
		Todo: "graph.Custom is gone; check the value in the handler and return nexus.Invalid().Field(…)"},
	{From: "", Old: "WithArgValidator",
		Todo: "nexus.WithArgValidator is gone; use validate:\"…\" tags, and return nexus.Invalid().Field(…) from the handler for other checks"},
	{From: "", Old: "RegisterGqlType",
		Todo: "nexus.RegisterGqlType takes a Go type now: RegisterGqlType[T](name) for an input object, RegisterGqlType(name, values…) for an enum"},
	{From: "", Old: "GqlField",
		Todo: "nexus.GqlField is internal; register GraphQL fields with nexus.AsQuery / nexus.AsMutation"},
	{From: "", Old: "GqlFieldGroup",
		Todo: "nexus.GqlFieldGroup is internal; register GraphQL fields with nexus.AsQuery / nexus.AsMutation"},
	{From: "transport/gql", Old: "Mount",
		Todo: "mounting a hand-built schema is gone (as is Service.MountGraphQL); register the fields with nexus.AsQuery / nexus.AsMutation"},
	{From: "transport/gql", Old: "SetStatusCode", To: "", New: "SetGraphStatus"},
}

// migrateSymbolTable renders the table for `nexus migrate v2 --help`.
func migrateSymbolTable(table []migrateSymbol) string {
	var b strings.Builder
	for _, s := range table {
		old := symbolPkgName(s.From) + "." + s.Old
		if s.Todo != "" {
			fmt.Fprintf(&b, "  %-30s TODO: %s\n", old, s.Todo)
			continue
		}
		fmt.Fprintf(&b, "  %-30s -> %s.%s\n", old, symbolPkgName(s.To), s.New)
	}
	return b.String()
}

// migrateTodoPrefix starts every comment the codemod leaves for a manual step.
const migrateTodoPrefix = "// TODO(nexus v2): "

// symbolPkgPath is the v2 import path of a module-relative package.
func symbolPkgPath(rel string) string {
	if rel == "" {
		return nexusModule + "/v2"
	}
	return nexusModule + "/v2/" + rel
}

// symbolPkgName is the package name a module-relative package is declared
// with (and so the name an unaliased import binds).
func symbolPkgName(rel string) string {
	if rel == "" {
		return "nexus"
	}
	return path.Base(rel)
}

// nexusRelPkg maps an import path (v1 or v2) to its package relative to the
// nexus root module; ok is false outside it (including the submodules,
// which keep their own symbols).
func nexusRelPkg(importPath string) (string, bool) {
	p := importPath
	if v2, ok := migrateImportPath(p); ok {
		p = v2
	}
	root := nexusModule + "/v2"
	if p == root {
		return "", true
	}
	rest, ok := strings.CutPrefix(p, root+"/")
	return rest, ok
}

// migrateGoSymbols rewrites uses of moved and renamed symbols (see
// migrateV2Symbols) in a Go file. Only selectors whose X is the local name
// of a nexus import — an alias included, a shadowing local variable not —
// are rewritten. The target package is imported when needed (aliased as
// nexus<name> when the plain name is taken in the file), and an import the
// rewrite left unused is dropped.
func migrateGoSymbols(_ string, src []byte) ([]byte, []migrateChange, error) {
	return rewriteGoSymbols(src, migrateV2Symbols)
}

func rewriteGoSymbols(src []byte, table []migrateSymbol) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	// Local import name → module-relative package, for nexus imports.
	local := map[string]string{}
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		rel, ok := nexusRelPkg(p)
		if !ok {
			continue
		}
		name := symbolPkgName(rel)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		local[name] = rel
	}
	if len(local) == 0 {
		return src, nil, nil
	}
	type key struct{ pkg, name string }
	rows := map[key]migrateSymbol{}
	for _, s := range table {
		rows[key{s.From, s.Old}] = s
	}

	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	var changes []migrateChange
	targets := map[string]string{} // module-relative target package → name used in the file
	todoAt := map[int]bool{}       // statement offsets already given a TODO
	offset := func(p token.Pos) int { return fset.Position(p).Offset }

	targetName := func(rel string) string {
		if n, ok := targets[rel]; ok {
			return n
		}
		n := importedAs(f, symbolPkgPath(rel))
		if n == "" {
			n = symbolPkgName(rel)
			if nameTaken(f, n, symbolPkgPath(rel)) {
				n = "nexus" + n
			}
		}
		targets[rel] = n
		return n
	}

	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Obj != nil { // Obj set: a local declaration shadows the import
			return true
		}
		from, ok := local[id.Name]
		if !ok {
			return true
		}
		row, ok := rows[key{from, sel.Sel.Name}]
		if !ok {
			return true
		}
		line := fset.Position(sel.Pos()).Line
		old := id.Name + "." + sel.Sel.Name
		if row.Todo != "" {
			path, _ := astutil.PathEnclosingInterval(f, sel.Pos(), sel.End())
			if start, end, ok := listElement(fset, path); ok {
				edits = append(edits, edit{start, end, ""})
				changes = append(changes, migrateChange{Line: line, Old: old + "(…)", New: "TODO comment"})
			} else {
				changes = append(changes, migrateChange{Line: line, Old: old, New: "TODO comment"})
			}
			if at, indent, ok := todoAnchor(fset, src, path); ok && !todoAt[at] && !hasTodoAbove(src, at, row.Todo) {
				todoAt[at] = true
				edits = append(edits, edit{at, at, indent + migrateTodoPrefix + row.Todo + "\n"})
			}
			return false
		}
		nw := targetName(row.To) + "." + row.New
		edits = append(edits, edit{offset(sel.Pos()), offset(sel.End()), nw})
		changes = append(changes, migrateChange{Line: line, Old: old, New: nw})
		return false
	})
	if len(edits) == 0 {
		return src, nil, nil
	}

	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b bytes.Buffer
	last := 0
	for _, e := range edits {
		if e.start < last { // nested in an edit already applied (a dropped element)
			continue
		}
		b.Write(src[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.Write(src[last:])

	// Fix the import block on the rewritten source.
	fset = token.NewFileSet()
	f, err = parser.ParseFile(fset, "", b.Bytes(), parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	for rel, name := range targets {
		p := symbolPkgPath(rel)
		if importedAs(f, p) != "" {
			continue
		}
		alias := ""
		if name != symbolPkgName(rel) {
			alias = name
		}
		astutil.AddNamedImport(fset, f, alias, p)
	}
	for _, spec := range append([]*ast.ImportSpec(nil), f.Imports...) {
		p, _ := strconv.Unquote(spec.Path.Value)
		rel, ok := nexusRelPkg(p)
		if !ok {
			continue
		}
		name := symbolPkgName(rel)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." || usesPackage(f, name) {
			continue
		}
		alias := ""
		if spec.Name != nil {
			alias = spec.Name.Name
		}
		astutil.DeleteNamedImport(fset, f, alias, p)
	}
	// A block left holding one import prints as `import "p"`, as gofmt'd
	// code would spell it.
	for _, d := range f.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.IMPORT && len(gd.Specs) == 1 && gd.Lparen.IsValid() {
			if spec := gd.Specs[0].(*ast.ImportSpec); spec.Doc == nil && spec.Comment == nil {
				gd.Lparen, gd.Rparen = token.NoPos, token.NoPos
			}
		}
	}
	var out bytes.Buffer
	if err := format.Node(&out, fset, f); err != nil {
		return nil, nil, err
	}
	return out.Bytes(), changes, nil
}

// importedAs returns the name the file binds importPath to, or "".
func importedAs(f *ast.File, importPath string) string {
	for _, spec := range f.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p != importPath {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				return ""
			}
			return spec.Name.Name
		}
		rel, _ := nexusRelPkg(importPath)
		return symbolPkgName(rel)
	}
	return ""
}

// nameTaken reports whether name is already bound in the file to something
// other than importPath: another import, or any identifier the file declares
// or uses under that name (a variable named config, say).
func nameTaken(f *ast.File, name, importPath string) bool {
	for _, spec := range f.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		if p == importPath {
			continue
		}
		bound := path.Base(p)
		if rel, ok := nexusRelPkg(p); ok {
			bound = symbolPkgName(rel)
		}
		if spec.Name != nil {
			bound = spec.Name.Name
		}
		if bound == name {
			return true
		}
	}
	taken := false
	ast.Inspect(f, func(n ast.Node) bool {
		if taken {
			return false
		}
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr:
			ast.Inspect(n.X, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == name {
					taken = true
				}
				return !taken
			})
			return false // Sel names a field or method, not a binding
		case *ast.Ident:
			if n.Name == name {
				taken = true
			}
		}
		return true
	})
	return taken
}

// usesPackage reports whether the file refers to an import bound to name.
func usesPackage(f *ast.File, name string) bool {
	used := false
	ast.Inspect(f, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && id.Obj == nil {
				used = true
			}
		}
		return !used
	})
	return used
}

// listElement finds, for a selector path ending in a call (sel → call),
// whether that call is an element of an argument list or a composite
// literal, and returns the byte span that drops it along with one
// separating comma.
func listElement(fset *token.FileSet, path []ast.Node) (start, end int, ok bool) {
	// path[0] is the selector; path[1] the call it is the Fun of.
	if len(path) < 3 {
		return 0, 0, false
	}
	call, ok := path[1].(*ast.CallExpr)
	if !ok || call.Fun != path[0] {
		return 0, 0, false
	}
	var elts []ast.Expr
	var open, close token.Pos
	switch parent := path[2].(type) {
	case *ast.CallExpr:
		elts, open, close = parent.Args, parent.Lparen, parent.Rparen
	case *ast.CompositeLit:
		elts, open, close = parent.Elts, parent.Lbrace, parent.Rbrace
	default:
		return 0, 0, false
	}
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	for i, e := range elts {
		if e != ast.Expr(call) {
			continue
		}
		switch {
		case len(elts) == 1:
			return off(open) + 1, off(close), true
		case i < len(elts)-1:
			return off(e.Pos()), off(elts[i+1].Pos()), true
		default:
			return off(elts[i-1].End()), off(e.End()), true
		}
	}
	return 0, 0, false
}

// todoAnchor finds where a TODO comment for a use goes: the start of the
// line of its innermost statement that sits in a statement list, else of
// its top-level declaration. It returns that offset and the line's indent.
func todoAnchor(fset *token.FileSet, src []byte, path []ast.Node) (int, string, bool) {
	var anchor ast.Node
	for i := 0; i+1 < len(path) && anchor == nil; i++ {
		n, parent := path[i], path[i+1]
		switch parent.(type) {
		case *ast.BlockStmt, *ast.CaseClause, *ast.CommClause:
			if _, ok := n.(ast.Stmt); ok {
				anchor = n
			}
		case *ast.File:
			if _, ok := n.(ast.Decl); ok {
				anchor = n
			}
		}
	}
	if anchor == nil {
		return 0, "", false
	}
	at := fset.Position(anchor.Pos()).Offset
	lineStart := bytes.LastIndexByte(src[:at], '\n') + 1
	indent := src[lineStart:at]
	if len(bytes.TrimLeft(indent, " \t")) != 0 {
		return 0, "", false // the statement shares its line with other code
	}
	return lineStart, string(indent), true
}

// hasTodoAbove reports whether the line before at already carries the TODO.
func hasTodoAbove(src []byte, at int, todo string) bool {
	if at == 0 {
		return false
	}
	prevStart := bytes.LastIndexByte(src[:at-1], '\n') + 1
	return strings.Contains(string(src[prevStart:at]), migrateTodoPrefix+todo)
}

// uriTagKey matches a uri:"…" pair inside a struct tag.
var uriTagKey = regexp.MustCompile(`(^|\s)uri:("[^"]*")`)

// migrateGoTags rewrites the retired uri:"x" struct tag to path:"x" in Go
// files that import nexus (a field already carrying path: just loses its
// uri: pair). Only raw-string (backquoted) tags are touched.
func migrateGoTags(_ string, src []byte) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	importsNexus := false
	for _, spec := range f.Imports {
		if p, _ := strconv.Unquote(spec.Path.Value); p == nexusModule || strings.HasPrefix(p, nexusModule+"/") {
			importsNexus = true
		}
	}
	if !importsNexus {
		return src, nil, nil
	}
	var b bytes.Buffer
	var changes []migrateChange
	last := 0
	ast.Inspect(f, func(n ast.Node) bool {
		field, ok := n.(*ast.Field)
		if !ok || field.Tag == nil || !strings.HasPrefix(field.Tag.Value, "`") {
			return true
		}
		tag := field.Tag.Value
		body := tag[1 : len(tag)-1]
		if !uriTagKey.MatchString(body) {
			return true
		}
		_, hasPath := reflect.StructTag(body).Lookup("path")
		nw := uriTagKey.ReplaceAllStringFunc(body, func(m string) string {
			if hasPath {
				return ""
			}
			sub := uriTagKey.FindStringSubmatch(m)
			return sub[1] + "path:" + sub[2]
		})
		nw = "`" + strings.TrimSpace(nw) + "`"
		start := fset.Position(field.Tag.Pos()).Offset
		b.Write(src[last:start])
		b.WriteString(nw)
		last = fset.Position(field.Tag.End()).Offset
		changes = append(changes, migrateChange{Line: fset.Position(field.Tag.Pos()).Line, Old: tag, New: nw})
		return true
	})
	if len(changes) == 0 {
		return src, nil, nil
	}
	b.Write(src[last:])
	return b.Bytes(), changes, nil
}
