package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/tools/go/packages"
)

// newGenCmd builds the `nexus gen` command and its subcommands. Today
// only `nexus gen clients` is wired; the parent kept as a group so
// future generators (e.g. nexus gen openapi) slot in without renaming
// anything.
func newGenCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gen <subcommand>",
		Short: "Code generators",
	}
	cmd.AddCommand(newGenClientsCmd(stdout, stderr))
	return cmd
}

func newGenClientsCmd(stdout, stderr io.Writer) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "clients [packages]",
		Short: "Generate cross-module client stubs from DeployAs-tagged modules",
		Long: `Walk every nexus.Module(...) declaration in [packages] (default ./...) and
emit one zz_<module>_client_gen.go file per module marked with DeployAs.

Generated clients pick the local in-process invoker when this binary
owns the module's deployment, or the HTTP RemoteCaller (reading
<TAG>_URL) when it doesn't — same call sites work in both shapes.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pattern := "./..."
			if len(args) > 0 {
				pattern = args[0]
			}
			return runGenClients(pattern, dryRun, stdout, stderr)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"print generated files to stdout instead of writing them")
	return cmd
}

// runGenClients does the actual work — separated from the cobra wrapper
// so unit tests can drive it directly.
func runGenClients(pattern string, dryRun bool, stdout, stderr io.Writer) error {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedDeps |
			packages.NeedImports,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return fmt.Errorf("load packages: %w", err)
	}
	if hasErrors(pkgs) {
		// Report parse/type errors but keep going — most "gen clients"
		// runs against a project that hasn't fully built yet still
		// have enough info to generate.
		for _, p := range pkgs {
			for _, e := range p.Errors {
				fmt.Fprintf(stderr, "warn: %s\n", e)
			}
		}
	}

	mods := scanModules(pkgs)
	if len(mods) == 0 {
		fmt.Fprintln(stdout, "nexus gen clients: no nexus.Module(...) declarations with DeployAs found in", pattern)
		return nil
	}

	var any bool
	for _, m := range mods {
		if m.Tag == "" {
			continue // un-deployable modules don't get clients
		}
		if len(m.Endpoints) == 0 {
			fmt.Fprintf(stderr, "nexus gen clients: module %q tagged %q has no AsRest endpoints — skipping\n", m.Name, m.Tag)
			continue
		}
		out, err := renderClient(m)
		if err != nil {
			return fmt.Errorf("render %q: %w", m.Name, err)
		}
		dest := filepath.Join(m.PackageDir, fmt.Sprintf("zz_%s_client_gen.go", strings.ToLower(m.Name)))
		if dryRun {
			fmt.Fprintf(stdout, "// === %s ===\n%s\n", dest, out)
		} else {
			if err := os.WriteFile(dest, out, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", dest, err)
			}
			fmt.Fprintf(stdout, "wrote %s (%d methods)\n", dest, len(m.Endpoints))
		}
		any = true
	}
	if !any {
		fmt.Fprintln(stdout, "nexus gen clients: nothing to generate (no DeployAs-tagged modules with REST endpoints)")
	}
	return nil
}

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

// --- rendering ---

// clientTpl is the body of a generated client file. text/template
// keeps the layout legible and lets the per-method block stay simple
// — we run gofmt on the result so spacing differences don't matter.
const clientTpl = `// Code generated by 'nexus gen clients'; DO NOT EDIT.
// To regenerate, run: nexus gen clients ./...

package {{.Package}}

import (
	"context"

	"github.com/paulmanoni/nexus"
)

// {{.ClientType}} is the typed client surface for the {{printf "%q" .ModuleName}} module.
// One method per AsRest / AsQuery / AsMutation handler in that
// module's declaration. The implementation is selected at construction
// time based on the running binary's deployment.
type {{.ClientType}} interface {
{{- range .Endpoints}}
	{{.OpName}}(ctx context.Context{{if .HasArgs}}, args {{.ArgsType}}{{end}}) {{methodReturn .}}
{{- end}}
}

// New{{.ClientType}} returns the appropriate implementation for the
// running binary:
//   - In-process LocalInvoker when this binary owns the {{printf "%q" .Tag}}
//     deployment, OR when no deployment is set (monolith mode).
//   - HTTP RemoteCaller reading {{.EnvVar}} otherwise. The local
//     binary's version is threaded in so RemoteCaller can detect
//     peer-version skew on the first call (single warning line, no
//     fail-fast).
func New{{.ClientType}}(app *nexus.App) {{.ClientType}} {
	if dep := app.Deployment(); dep == "" || dep == {{printf "%q" .Tag}} {
		return &{{.LocalImpl}}{call: nexus.NewLocalInvoker(app)}
	}
	return &{{.RemoteImpl}}{call: nexus.NewRemoteCallerFromEnv(
		{{printf "%q" .EnvVar}},
		nexus.WithLocalVersion(app.Version()),
	)}
}

type {{.LocalImpl}} struct{ call nexus.ClientCallable }
{{range .Endpoints}}
func (c *{{$.LocalImpl}}) {{.OpName}}(ctx context.Context{{if .HasArgs}}, args {{.ArgsType}}{{end}}) {{methodReturn .}} {
{{- methodBody . "c.call" }}
}
{{end}}
type {{.RemoteImpl}} struct{ call nexus.ClientCallable }
{{range .Endpoints}}
func (c *{{$.RemoteImpl}}) {{.OpName}}(ctx context.Context{{if .HasArgs}}, args {{.ArgsType}}{{end}}) {{methodReturn .}} {
{{- methodBody . "c.call" }}
}
{{end}}
`

// renderClient turns one modInfo into a gofmt-clean Go source file.
func renderClient(m modInfo) ([]byte, error) {
	clientType := goExport(m.Name) + "Client"
	data := struct {
		Package    string
		ModuleName string
		Tag        string
		EnvVar     string
		ClientType string
		LocalImpl  string
		RemoteImpl string
		Endpoints  []endpointInfo
	}{
		Package:    m.Package,
		ModuleName: m.Name,
		Tag:        m.Tag,
		EnvVar:     envVarFromTag(m.Tag),
		ClientType: clientType,
		LocalImpl:  unexport(clientType) + "Local",
		RemoteImpl: unexport(clientType) + "Remote",
		Endpoints:  m.Endpoints,
	}
	// Stable order so re-runs produce identical output (test-friendly).
	sort.Slice(data.Endpoints, func(i, j int) bool {
		return data.Endpoints[i].OpName < data.Endpoints[j].OpName
	})

	tpl := template.Must(template.New("client").Funcs(template.FuncMap{
		"methodReturn": func(e endpointInfo) string {
			if e.HasReturn {
				return "(" + e.Return + ", error)"
			}
			return "error"
		},
		"argExpr": func(e endpointInfo) string {
			if e.HasArgs {
				return "args"
			}
			return "nil"
		},
		"methodBody": renderMethodBody,
	}).Parse(clientTpl))
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		// On format failure, emit the unformatted source so the user
		// can see what we tried to write — useful for diagnosing
		// template bugs against unfamiliar input shapes.
		return buf.Bytes(), fmt.Errorf("gofmt: %w (raw output retained)", err)
	}
	return formatted, nil
}

// renderMethodBody emits the per-method body. Dispatches on the
// endpoint's transport so REST goes through ClientCallable.Invoke,
// GraphQL ops go through nexus.GqlCall[T], and WS — once supported —
// will get its own branch. Returns a string spliced into the
// generated source; gofmt cleans up indentation afterwards.
//
// recv is the receiver expression used inside the method (e.g.
// "c.call"). Different impls share the same template by passing
// their own.
func renderMethodBody(e endpointInfo, recv string) string {
	switch e.Transport {
	case transportGqlQuery:
		return renderGqlBody(e, recv, "query")
	case transportGqlMutation:
		return renderGqlBody(e, recv, "mutation")
	case transportWS:
		// WS clients are deferred to v2 — generate a clear runtime
		// error so the call site fails loud rather than silently
		// degrading. The shape is still emitted so consumers can
		// implement against the interface today and only need to
		// regenerate when WS support lands.
		if e.HasReturn {
			return "\n\tvar zero " + e.Return + "\n\treturn zero, " +
				"nexusErrWSNotImplemented(" + strconv.Quote(e.Method) + ")\n"
		}
		return "\n\treturn nexusErrWSNotImplemented(" + strconv.Quote(e.Method) + ")\n"
	default: // transportREST
		return renderRestBody(e, recv)
	}
}

func renderRestBody(e endpointInfo, recv string) string {
	method := strconv.Quote(e.Method)
	path := strconv.Quote(e.Path)
	args := "nil"
	if e.HasArgs {
		args = "args"
	}
	if e.HasReturn {
		return "\n\tvar out " + e.Return + "\n\terr := " + recv +
			".Invoke(ctx, " + method + ", " + path + ", " + args + ", &out)\n\treturn out, err\n"
	}
	return "\n\treturn " + recv + ".Invoke(ctx, " + method + ", " + path + ", " + args + ", nil)\n"
}

func renderGqlBody(e endpointInfo, recv, opType string) string {
	args := "nil"
	if e.HasArgs {
		args = "args"
	}
	gqlName := strconv.Quote(e.GqlName)
	if e.HasReturn {
		return "\n\treturn nexus.GqlCall[" + e.Return + "](ctx, " + recv +
			", " + strconv.Quote(opType) + ", " + gqlName + ", " + args + ", nexus.GqlOptions{})\n"
	}
	// GraphQL ops with no return value are unusual; emit a discard.
	return "\n\t_, err := nexus.GqlCall[any](ctx, " + recv +
		", " + strconv.Quote(opType) + ", " + gqlName + ", " + args + ", nexus.GqlOptions{})\n\treturn err\n"
}

// envVarFromTag turns "users-svc" into "USERS_SVC_URL". Convention:
// uppercase + replace `-` with `_`, append _URL.
func envVarFromTag(tag string) string {
	out := strings.ToUpper(tag)
	out = strings.ReplaceAll(out, "-", "_")
	return out + "_URL"
}

// goExport / unexport adjust the case of an identifier's first rune.
// Used to derive client type names ("users" → "Users") and impl names
// ("UsersClient" → "usersClient").
func goExport(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func unexport(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}