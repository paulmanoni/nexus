package main

// gen.go used to host the `nexus gen clients` subcommand that wrote
// per-module zz_*_client_gen.go files (the UsersClient/Local/Remote
// codegen path). Path 3 replaced that approach with overlay-driven
// shadow generation in build.go: the consumer's struct field is
// `*users.Service` directly, and `nexus build --deployment X`
// produces the right Service body per binary.
//
// What remains in this file is the AST-scanning infrastructure
// (scanModules, parseModuleCall, parseAsRestCall, fillSignature,
// modInfo, endpointInfo, transportKind) — still consumed by
// build.go's runBuild and dev_split.go's discoverDeployTags. The
// rendering side (clientTpl, renderClient, renderMethodBody) is
// gone.

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

func hasErrors(pkgs []*packages.Package) bool {
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			return true
		}
	}
	return false
}

// --- model ---

// modInfo is one DeployAs-tagged nexus.Module discovered in the input.
// Endpoints are AsRest registrations whose handler we successfully
// resolved to a typed function in this or an imported package.
type modInfo struct {
	Name       string         // first arg of nexus.Module(...)
	Tag        string         // arg of nexus.DeployAs(...) within the same Module call
	Package    string         // name of the destination Go package
	PackageDir string         // filesystem path of the destination package
	PackagePath string        // import path of the destination package
	Endpoints  []endpointInfo // resolved AsRest registrations
}

// transportKind tags an endpoint's wire shape so the renderer picks
// the right body template (REST vs GraphQL vs WebSocket).
type transportKind int

const (
	transportREST transportKind = iota
	transportGqlQuery
	transportGqlMutation
	transportWS
)

// endpointInfo is one AsRest / AsQuery / AsMutation / AsWS call whose
// handler we resolved.
type endpointInfo struct {
	Transport transportKind
	Method    string // HTTP verb literal (REST); message type (WS); empty for GraphQL
	Path      string // REST/WS path; empty for GraphQL (uses the framework's mount)
	OpName    string // exported method name on the generated client
	GqlName   string // GraphQL field name (lowercased opName) — used in query string
	ArgsType  string // Go syntax for the args type, qualified for the destination package
	HasArgs   bool   // false → method takes only ctx
	Return    string // Go syntax for the return type (T from Params[T] handler)
	HasReturn bool   // false → handler returned only error → method returns just error
}

// discoverDeployTags is a thin wrapper around scanModules that returns
// just the unique, non-empty DeployAs tags discovered under pattern.
// `nexus dev --split` uses it to enumerate the deployment units it
// needs to launch as subprocesses without otherwise caring about the
// AsRest signatures that gen clients reads.
func discoverDeployTags(pattern string) ([]string, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []string
	for _, m := range scanModules(pkgs) {
		if m.Tag == "" {
			continue
		}
		if _, dup := seen[m.Tag]; dup {
			continue
		}
		seen[m.Tag] = struct{}{}
		out = append(out, m.Tag)
	}
	sort.Strings(out)
	return out, nil
}

// scanModules walks every package's AST and collects nexus.Module(...)
// calls with their DeployAs tag and AsRest endpoints. Returns a flat
// slice; the caller groups by tag if needed.
func scanModules(pkgs []*packages.Package) []modInfo {
	var out []modInfo
	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		for _, file := range p.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isNexusCall(call.Fun, "Module") {
					return true
				}
				m := parseModuleCall(p, call)
				if m == nil {
					return true
				}
				out = append(out, *m)
				return true
			})
		}
	}
	return out
}

// isNexusCall checks whether expr is a call to a function named
// `name` in the nexus package (selector form: nexus.<name>). Catches
// both unqualified imports (rare) and renamed imports.
func isNexusCall(expr ast.Expr, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	// Resolve via Obj when present (the import alias is bound to a
	// package). Falls back to literal "nexus" for the common case.
	if id.Obj == nil {
		return id.Name == "nexus"
	}
	return id.Name == "nexus" || strings.HasSuffix(id.Name, "nexus")
}

// parseModuleCall extracts (name, tag, endpoints) from one Module call.
// Returns nil when the call's first arg isn't a string literal —
// dynamically-named modules can't drive codegen.
func parseModuleCall(p *packages.Package, call *ast.CallExpr) *modInfo {
	if len(call.Args) < 2 {
		return nil
	}
	name, ok := stringLit(call.Args[0])
	if !ok {
		return nil
	}
	m := modInfo{
		Name:        name,
		Package:     p.Name,
		PackagePath: p.PkgPath,
	}
	if len(p.GoFiles) > 0 {
		m.PackageDir = filepath.Dir(p.GoFiles[0])
	}
	for _, opt := range call.Args[1:] {
		opCall, ok := opt.(*ast.CallExpr)
		if !ok {
			continue
		}
		switch {
		case isNexusCall(opCall.Fun, "DeployAs"):
			if len(opCall.Args) >= 1 {
				if tag, ok := stringLit(opCall.Args[0]); ok {
					m.Tag = tag
				}
			}
		case isNexusCall(opCall.Fun, "AsRest"):
			ep := parseAsRestCall(p, opCall)
			if ep != nil {
				m.Endpoints = append(m.Endpoints, *ep)
			}
		case isNexusCall(opCall.Fun, "AsQuery"):
			ep := parseAsGqlCall(p, opCall, transportGqlQuery)
			if ep != nil {
				m.Endpoints = append(m.Endpoints, *ep)
			}
		case isNexusCall(opCall.Fun, "AsMutation"):
			ep := parseAsGqlCall(p, opCall, transportGqlMutation)
			if ep != nil {
				m.Endpoints = append(m.Endpoints, *ep)
			}
		case isNexusCall(opCall.Fun, "AsWS"):
			ep := parseAsWSCall(p, opCall)
			if ep != nil {
				m.Endpoints = append(m.Endpoints, *ep)
			}
		}
	}
	return &m
}

// parseAsRestCall extracts (method, path, fn) from one AsRest call.
// fn must resolve to a typed function in this package's program;
// dynamic factories or inline closures are skipped silently — the
// generator surfaces a warning above when a tagged module produces no
// endpoints, so misses don't go unnoticed.
func parseAsRestCall(p *packages.Package, call *ast.CallExpr) *endpointInfo {
	if len(call.Args) < 3 {
		return nil
	}
	method, ok := stringLit(call.Args[0])
	if !ok {
		return nil
	}
	path, ok := stringLit(call.Args[1])
	if !ok {
		return nil
	}
	fn := resolveFunc(p, call.Args[2])
	if fn == nil {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil
	}
	ep := endpointInfo{
		Transport: transportREST,
		Method:    strings.ToUpper(method),
		Path:      path,
		OpName:    opNameFromGoIdent(fn.Name()),
	}
	fillSignature(p, sig, &ep)
	return &ep
}

// parseAsGqlCall extracts (handler) from one AsQuery / AsMutation
// call and returns it as an endpointInfo with the matching transport
// kind. Args + return type are reflected from the handler signature
// the same way parseAsRestCall does — same conventions, same
// constraints (top-level handler, no closures).
//
// AsQuery/AsMutation share their first arg as the handler:
//
//	nexus.AsQuery(NewListPets, opts...)
//
// — no method/path string. The op name comes from the handler's Go
// identifier (matching the runtime's opNameFromFunc).
func parseAsGqlCall(p *packages.Package, call *ast.CallExpr, kind transportKind) *endpointInfo {
	if len(call.Args) < 1 {
		return nil
	}
	fn := resolveFunc(p, call.Args[0])
	if fn == nil {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil
	}
	op := opNameFromGoIdent(fn.Name())
	ep := endpointInfo{
		Transport: kind,
		OpName:    op,
		GqlName:   lowerFirst(op),
	}
	fillSignature(p, sig, &ep)
	return &ep
}

// parseAsWSCall extracts (path, msgType, handler) from one AsWS call.
// The generated client method is named after the handler with the
// same exported-camelCase rules as the other transports; the path +
// msgType land on Path/Method so the runtime helper knows what
// envelope to send.
//
//	nexus.AsWS("/events", "chat.send", NewChatSend, opts...)
func parseAsWSCall(p *packages.Package, call *ast.CallExpr) *endpointInfo {
	if len(call.Args) < 3 {
		return nil
	}
	path, ok := stringLit(call.Args[0])
	if !ok {
		return nil
	}
	msgType, ok := stringLit(call.Args[1])
	if !ok {
		return nil
	}
	fn := resolveFunc(p, call.Args[2])
	if fn == nil {
		return nil
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return nil
	}
	ep := endpointInfo{
		Transport: transportWS,
		Method:    msgType,
		Path:      path,
		OpName:    opNameFromGoIdent(fn.Name()),
	}
	fillSignature(p, sig, &ep)
	return &ep
}

// fillSignature is the args/return type extraction shared by AsRest,
// AsQuery, AsMutation, and AsWS parsers. Looks for Params[T] anywhere
// in the params list (preferred); falls back to a trailing struct
// param for legacy handlers. Picks the first non-error return as the
// result type.
func fillSignature(p *packages.Package, sig *types.Signature, ep *endpointInfo) {
	q := makeQualifier(p)
	var argsType types.Type
	for i := 0; i < sig.Params().Len(); i++ {
		pt := sig.Params().At(i).Type()
		if t := paramsTypeArg(pt); t != nil {
			argsType = t
			break
		}
	}
	if argsType == nil && sig.Params().Len() > 0 {
		last := sig.Params().At(sig.Params().Len() - 1).Type()
		if isStruct(last) {
			argsType = last
		}
	}
	if argsType != nil {
		ep.HasArgs = true
		ep.ArgsType = types.TypeString(argsType, q)
	}
	for i := 0; i < sig.Results().Len(); i++ {
		rt := sig.Results().At(i).Type()
		if isErrorType(rt) {
			continue
		}
		ep.Return = types.TypeString(rt, q)
		ep.HasReturn = true
		break
	}
}

// lowerFirst returns s with its first ASCII rune lowercased. Used
// to derive the GraphQL field name ("ListPets" → "listPets") matching
// the runtime's opNameFromFunc convention.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'A' && c <= 'Z' {
		return string(c+('a'-'A')) + s[1:]
	}
	return s
}

// resolveFunc walks an expression to a *types.Func declaration. Today
// only handles the common case of an Ident or a *ast.SelectorExpr
// pointing at a top-level function. Closures, method values, etc. are
// out of scope for v1 — those handlers can't generate clients without
// extra metadata anyway.
func resolveFunc(p *packages.Package, expr ast.Expr) *types.Func {
	switch e := expr.(type) {
	case *ast.Ident:
		obj := p.TypesInfo.ObjectOf(e)
		if fn, ok := obj.(*types.Func); ok {
			return fn
		}
	case *ast.SelectorExpr:
		obj := p.TypesInfo.ObjectOf(e.Sel)
		if fn, ok := obj.(*types.Func); ok {
			return fn
		}
	}
	return nil
}

// paramsTypeArg returns the T inside a nexus.Params[T] type, or nil
// when the type isn't a Params instantiation. Recognizes the framework's
// generic by import path + name to stay robust against renamed imports.
func paramsTypeArg(t types.Type) types.Type {
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return nil
	}
	if obj.Pkg().Path() != "github.com/paulmanoni/nexus" || obj.Name() != "Params" {
		return nil
	}
	args := named.TypeArgs()
	if args == nil || args.Len() == 0 {
		return nil
	}
	return args.At(0)
}

func isStruct(t types.Type) bool {
	if named, ok := t.(*types.Named); ok {
		return isStruct(named.Underlying())
	}
	_, ok := t.(*types.Struct)
	return ok
}

func isErrorType(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	return named.Obj() != nil && named.Obj().Pkg() == nil && named.Obj().Name() == "error"
}

// makeQualifier returns a function suitable for types.TypeString that
// renders types from the destination package as bare names and others
// as `pkg.Name`. Used so generated code that lives in the same package
// as the args/return types doesn't import itself.
func makeQualifier(dest *packages.Package) types.Qualifier {
	return func(p *types.Package) string {
		if p == nil {
			return ""
		}
		if dest != nil && dest.Types != nil && p.Path() == dest.Types.Path() {
			return ""
		}
		return p.Name()
	}
}

// opNameFromGoIdent mirrors the framework's runtime opNameFromFunc but
// keeps the result *exported* — it's a Go method name on the client
// interface, not a graphql field. "NewGetUser" → "GetUser",
// "ListPets" → "ListPets".
func opNameFromGoIdent(name string) string {
	if strings.HasPrefix(name, "New") && len(name) > 3 && unicode.IsUpper(rune(name[3])) {
		return name[3:]
	}
	return name
}

func stringLit(expr ast.Expr) (string, bool) {
	bl, ok := expr.(*ast.BasicLit)
	if !ok {
		return "", false
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

