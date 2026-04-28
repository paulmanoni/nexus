package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// shadowFile is one logical→physical mapping the build emits into
// overlay.json. Original is the path the compiler thinks it's
// reading; Generated is where the rewritten/synthesized source
// actually lives (under .nexus/build/<deployment>/).
//
// Two flavors:
//   - REPLACE: an existing source file gets a stripped variant
//     (preserved types only). Original = on-disk path.
//   - ADD: a new file injected into the package (the shadow Service
//     + methods + Module + init). Original is a logical path under
//     the same package dir; nothing exists at that path on disk.
type shadowFile struct {
	Original  string // logical path (real or synthetic)
	Generated string // physical path under .nexus/build/<deployment>/
}

// renderShadowPackage walks every .go file in the module's package
// directory and produces the shadow file set:
//
//   - For each file, the preserved type declarations (everything
//     except `type Service`) are kept; Service struct, constructors,
//     handlers, Module, helpers are dropped. The stripped file is
//     overlay-replaced.
//   - One additional file (zz_shadow_gen.go) is overlay-added with
//     the stub Service definition, plain methods matching each
//     endpoint, a no-op Module declaration, and a RegisterAutoClient
//     init() block.
//
// The result handles both single-file modules (microsplit's current
// shape) and multi-file modules (types.go + service.go + handlers.go +
// module.go) uniformly: each file is processed independently, and
// the synthesized file slots in alongside.
func renderShadowPackage(m modInfo, projectRoot, deployment string) ([]shadowFile, error) {
	if m.PackageDir == "" {
		return nil, fmt.Errorf("module %q has no PackageDir", m.Name)
	}

	entries, err := os.ReadDir(m.PackageDir)
	if err != nil {
		return nil, fmt.Errorf("read package dir %s: %w", m.PackageDir, err)
	}

	var out []shadowFile
	for _, e := range entries {
		if e.IsDir() || !hasGoSuffix(e.Name()) {
			continue
		}
		// Skip codegen output (zz_*) and tests — both unwanted in
		// the shadow input.
		if isGeneratedName(e.Name()) || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		original := filepath.Join(m.PackageDir, e.Name())
		stripped, err := renderStrippedFile(m, original)
		if err != nil {
			return nil, fmt.Errorf("strip %s: %w", original, err)
		}
		generated, err := shadowOutputPath(projectRoot, deployment, original)
		if err != nil {
			return nil, fmt.Errorf("compute shadow path for %s: %w", original, err)
		}
		if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir for shadow %s: %w", generated, err)
		}
		if err := os.WriteFile(generated, stripped, 0o644); err != nil {
			return nil, fmt.Errorf("write shadow %s: %w", generated, err)
		}
		out = append(out, shadowFile{Original: original, Generated: generated})
	}

	// Synthesize the stub Service file. Logical path lives in the
	// same package dir under a generated name; nothing exists there
	// on disk.
	stubLogical := filepath.Join(m.PackageDir, "zz_shadow_gen.go")
	stubGenerated, err := shadowOutputPath(projectRoot, deployment, stubLogical)
	if err != nil {
		return nil, fmt.Errorf("compute stub path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(stubGenerated), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir for stub: %w", err)
	}
	stubBody, err := renderShadowStub(m)
	if err != nil {
		return nil, fmt.Errorf("render stub: %w", err)
	}
	if err := os.WriteFile(stubGenerated, stubBody, 0o644); err != nil {
		return nil, fmt.Errorf("write stub %s: %w", stubGenerated, err)
	}
	out = append(out, shadowFile{Original: stubLogical, Generated: stubGenerated})

	return out, nil
}

// renderStrippedFile parses one source file and emits a variant
// containing only:
//   - the package declaration
//   - exported type declarations (except `type Service`)
//
// Constructors, handlers, methods, Module declarations, helper
// functions, and unexported types all disappear. Imports are
// pruned to those still referenced by preserved types so the
// stripped file compiles.
//
// Empty results are valid: a file that holds only Service/handlers
// becomes `package <name>\n` — Go accepts that as a no-op file.
func renderStrippedFile(m modInfo, originalPath string) ([]byte, error) {
	fset := token.NewFileSet()
	src, err := os.ReadFile(originalPath)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(fset, originalPath, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	importsByLocalName := importsByName(f)
	// Track local names (the alias the file binds, e.g. `models2` for
	// `models2 "oats_applicant/models"`) so we can re-emit aliases
	// correctly. Keying by path would lose the alias and the
	// preserved type bodies' qualified references (`models2.Foo`)
	// would fail to resolve.
	usedLocalNames := map[string]struct{}{}

	var preserved []*ast.GenDecl
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		var keep []ast.Spec
		for _, sp := range gd.Specs {
			ts, ok := sp.(*ast.TypeSpec)
			if !ok {
				continue
			}
			// Service is always replaced by the synthesized stub.
			// Everything else — exported AND unexported types —
			// is preserved verbatim. Unexported types are package-
			// internal but still part of the package's identity,
			// and the shadow stub (also in this package) may
			// reference them in its method signatures (e.g. an
			// AsRest handler whose args type starts lowercase).
			if ts.Name.Name == "Service" {
				continue
			}
			collectImports(ts.Type, importsByLocalName, usedLocalNames)
			keep = append(keep, ts)
		}
		if len(keep) > 0 {
			cp := *gd
			cp.Specs = keep
			preserved = append(preserved, &cp)
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by 'nexus build' for shadow deployment — DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Stripped from: %s\n\n", originalPath)
	fmt.Fprintf(&b, "package %s\n", f.Name.Name)
	if len(usedLocalNames) > 0 {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "import (")
		for _, localName := range sortedKeys(usedLocalNames) {
			path := importsByLocalName[localName]
			if importNeedsAlias(localName, path) {
				fmt.Fprintf(&b, "\t%s %q\n", localName, path)
			} else {
				fmt.Fprintf(&b, "\t%q\n", path)
			}
		}
		fmt.Fprintln(&b, ")")
	}
	for _, decl := range preserved {
		fmt.Fprintln(&b)
		if decl.Doc != nil {
			for _, c := range decl.Doc.List {
				fmt.Fprintln(&b, c.Text)
			}
		}
		saved := decl.Doc
		decl.Doc = nil
		err := printer.Fprint(&b, fset, decl)
		decl.Doc = saved
		if err != nil {
			return nil, fmt.Errorf("print type decl: %w", err)
		}
		fmt.Fprintln(&b)
	}

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("gofmt stripped file: %w (raw output retained)", err)
	}
	return formatted, nil
}

// renderShadowStub emits the synthesized zz_shadow_gen.go file for
// a module: the stub Service struct (HTTP transport handle), the
// constructor that resolves the peer from Topology, plain methods
// matching each AsRest/AsQuery declaration, a no-op Module that
// preserves the DeployAs tag for topology validation, and an
// init() block that auto-registers the constructor.
//
// This is the moral equivalent of the old single-file shadow's
// "second half" — the part that's NEW source rather than stripped
// originals. Splitting it out lets multi-file packages have their
// originals stripped in place while one synthesized file holds
// the new declarations.
func renderShadowStub(m modInfo) ([]byte, error) {
	// Aggregate every external package the endpoint signatures
	// reference. Without this, methods returning `*pkg.Response`
	// compile to "undefined: pkg" in the stub.
	extraImports := map[string]bool{}
	for _, ep := range m.Endpoints {
		for _, imp := range ep.Imports {
			// Don't import the module's own package — that'd be a
			// self-import. fillSignature already excludes the dest
			// package, but the module's package path is hard to
			// reconstruct here without more plumbing; the heuristic
			// "skip when basename matches m.Package" is wrong (two
			// packages can share a basename). Trust fillSignature.
			extraImports[imp] = true
		}
	}
	extraImportPaths := make([]string, 0, len(extraImports))
	for p := range extraImports {
		extraImportPaths = append(extraImportPaths, p)
	}
	// Stable order across runs.
	for i := 1; i < len(extraImportPaths); i++ {
		for j := i; j > 0 && extraImportPaths[j-1] > extraImportPaths[j]; j-- {
			extraImportPaths[j-1], extraImportPaths[j] = extraImportPaths[j], extraImportPaths[j-1]
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by 'nexus build' for shadow deployment — DO NOT EDIT.\n")
	fmt.Fprintf(&b, "// Synthesized stub for module %q (DeployAs %q).\n\n", m.Name, m.Tag)
	fmt.Fprintf(&b, "package %s\n\n", m.Package)
	fmt.Fprintln(&b, "import (")
	fmt.Fprintln(&b, "\t\"context\"")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "\t\"github.com/paulmanoni/nexus\"")
	for _, p := range extraImportPaths {
		fmt.Fprintf(&b, "\t%q\n", p)
	}
	fmt.Fprintln(&b, ")")
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "// Service is the shadow definition: an HTTP-backed stub that\n")
	fmt.Fprintf(&b, "// replaces the local %s.Service struct in this build.\n", m.Package)
	fmt.Fprintln(&b, "type Service struct {")
	fmt.Fprintln(&b, "\tcall nexus.ClientCallable")
	fmt.Fprintln(&b, "}")
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "// NewService resolves the %q peer from Config.Topology and\n", m.Tag)
	fmt.Fprintln(&b, "// returns a Service whose methods route over HTTP via PeerCaller.")
	fmt.Fprintln(&b, "func NewService(app *nexus.App) *Service {")
	fmt.Fprintf(&b, "\tpeer, ok := app.Peer(%q)\n", m.Tag)
	fmt.Fprintln(&b, "\tif !ok {")
	fmt.Fprintf(&b, "\t\tpanic(`nexus: %s.Service requires Config.Topology.Peers[%q] — none declared`)\n", m.Package, m.Tag)
	fmt.Fprintln(&b, "\t}")
	fmt.Fprintln(&b, "\treturn &Service{")
	fmt.Fprintln(&b, "\t\tcall: nexus.NewPeerCaller(peer, nexus.WithLocalVersion(app.Version())),")
	fmt.Fprintln(&b, "\t}")
	fmt.Fprintln(&b, "}")
	fmt.Fprintln(&b)

	// Dedupe by OpName: a handler bound to multiple routes (e.g. an
	// OAuth endpoint registered as both POST and GET, or under two
	// paths) appears once per AsRest call in m.Endpoints. The shadow
	// stub names methods after the handler, so two registrations of
	// the same handler would emit `func (s *Service) X` twice and
	// fail to compile. The cross-module client only needs one entry
	// point per handler — both routes run identical code on the
	// peer — so keep the first occurrence.
	seenOp := make(map[string]struct{}, len(m.Endpoints))
	for _, ep := range m.Endpoints {
		if ep.OpName == "" {
			continue
		}
		if _, dup := seenOp[ep.OpName]; dup {
			continue
		}
		seenOp[ep.OpName] = struct{}{}
		writeShadowMethod(&b, ep)
	}

	fmt.Fprintln(&b, "// Module preserves the DeployAs tag for Topology validation;")
	fmt.Fprintln(&b, "// no routes register because this binary doesn't own the module.")
	fmt.Fprintf(&b, "var Module = nexus.Module(%q, nexus.DeployAs(%q))\n\n", m.Name, m.Tag)

	fmt.Fprintln(&b, "// init() runs at package load — BEFORE fx applies the")
	fmt.Fprintln(&b, "// NEXUS_DEPLOYMENT module filter — so the registrations land")
	fmt.Fprintln(&b, "// even when the shadow's enclosing Module would be filtered out.")
	fmt.Fprintln(&b, "// RegisterAutoClient lets consumer modules satisfy *Service via")
	fmt.Fprintln(&b, "// fx without a manual nexus.Provide line; RegisterRemoteServicePlaceholder")
	fmt.Fprintln(&b, "// makes the dashboard's Architecture tab show this peer module as a")
	fmt.Fprintln(&b, "// Remote-flagged card.")
	fmt.Fprintln(&b, "func init() {")
	fmt.Fprintln(&b, "\tnexus.RegisterAutoClient(NewService)")
	fmt.Fprintf(&b, "\tnexus.RegisterRemoteServicePlaceholder(%q, %q)\n", m.Name, m.Tag)
	fmt.Fprintln(&b, "}")

	formatted, err := format.Source(b.Bytes())
	if err != nil {
		return b.Bytes(), fmt.Errorf("gofmt shadow stub: %w (raw output retained)", err)
	}
	return formatted, nil
}

// writeShadowMethod emits one stub method on Service for an endpoint.
// REST endpoints route through ClientCallable.Invoke; GraphQL ops
// route through nexus.GqlCall[T].
//
// When ep.Skip is set (signature references an unexported type that
// can't be referenced cross-package anyway), the method is replaced
// with a one-line comment so the stub stays valid Go and the
// developer sees what was dropped from the cross-module surface.
func writeShadowMethod(b *bytes.Buffer, ep endpointInfo) {
	if ep.Skip {
		fmt.Fprintf(b, "// %s skipped: %s — not callable cross-module.\n", ep.OpName, ep.SkipReason)
		fmt.Fprintln(b)
		return
	}
	receiver := "s"
	args := "nil"
	argsParam := ""
	if ep.HasArgs {
		args = "args"
		argsParam = ", args " + ep.ArgsType
	}
	ret := "error"
	if ep.HasReturn {
		ret = "(" + ep.Return + ", error)"
	}
	fmt.Fprintf(b, "func (%s *Service) %s(ctx context.Context%s) %s {\n", receiver, ep.OpName, argsParam, ret)
	switch ep.Transport {
	case transportGqlQuery:
		writeGqlBody(b, ep, "query", receiver+".call", args)
	case transportGqlMutation:
		writeGqlBody(b, ep, "mutation", receiver+".call", args)
	case transportWS:
		if ep.HasReturn {
			fmt.Fprintf(b, "\tvar zero %s\n", ep.Return)
			fmt.Fprintf(b, "\treturn zero, fmt.Errorf(\"nexus shadow: WS endpoint %q not supported\")\n", ep.Method)
		} else {
			fmt.Fprintf(b, "\treturn fmt.Errorf(\"nexus shadow: WS endpoint %q not supported\")\n", ep.Method)
		}
	default: // REST
		writeRestBody(b, ep, receiver+".call", args)
	}
	fmt.Fprintln(b, "}")
	fmt.Fprintln(b)
}

func writeRestBody(b *bytes.Buffer, ep endpointInfo, recv, args string) {
	method := strconv.Quote(ep.Method)
	path := strconv.Quote(ep.Path)
	if ep.HasReturn {
		fmt.Fprintf(b, "\tvar out %s\n", ep.Return)
		fmt.Fprintf(b, "\terr := %s.Invoke(ctx, %s, %s, %s, &out)\n", recv, method, path, args)
		fmt.Fprintln(b, "\treturn out, err")
		return
	}
	fmt.Fprintf(b, "\treturn %s.Invoke(ctx, %s, %s, %s, nil)\n", recv, method, path, args)
}

func writeGqlBody(b *bytes.Buffer, ep endpointInfo, opType, recv, args string) {
	gqlName := strconv.Quote(ep.GqlName)
	if ep.HasReturn {
		fmt.Fprintf(b, "\treturn nexus.GqlCall[%s](ctx, %s, %s, %s, %s, nexus.GqlOptions{})\n",
			ep.Return, recv, strconv.Quote(opType), gqlName, args)
		return
	}
	fmt.Fprintf(b, "\t_, err := nexus.GqlCall[any](ctx, %s, %s, %s, %s, nexus.GqlOptions{})\n",
		recv, strconv.Quote(opType), gqlName, args)
	fmt.Fprintln(b, "\treturn err")
}

// importsByName indexes a file's import declarations by the package
// name they bind in this file (alias if explicit, otherwise the
// import path's last segment).
func importsByName(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			name = path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				name = path[i+1:]
			}
		}
		out[name] = path
	}
	return out
}

// collectImports walks a type expression and stamps any selector
// reference's *local name* onto the used set so the shadow's import
// block can re-emit the same alias the original file used. Keying by
// local name (e.g. `models2`) instead of path preserves aliases — a
// type referring to `models2.Foo` requires `models2 "path/to/models"`
// in the import block, not `"path/to/models"` (which Go would bind
// as `models`).
func collectImports(expr ast.Expr, imports map[string]string, usedLocalNames map[string]struct{}) {
	ast.Inspect(expr, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if _, ok := imports[id.Name]; ok {
			usedLocalNames[id.Name] = struct{}{}
		}
		return true
	})
}

// importNeedsAlias reports whether a localName differs from the
// default Go would derive from path (the path's last segment). When
// they match, the import line can omit the alias for cleaner output;
// when they differ (`models2 "path/to/models"`, `gosql "database/sql"`),
// the alias must be emitted to preserve resolution.
func importNeedsAlias(localName, path string) bool {
	defaultName := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		defaultName = path[i+1:]
	}
	return localName != defaultName
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// shadowOutputPath computes where to write the generated shadow
// source within the build cache. Layout:
//
//	<projectRoot>/.nexus/build/<deployment>/<modulePackagePath>/<basename>
//
// The basename matches the original file so go build -overlay
// substitution is one-to-one. For added (non-existent) originals,
// the same convention places the synthetic file in a path the
// compiler resolves alongside the package's other files.
func shadowOutputPath(projectRoot, deployment, originalPath string) (string, error) {
	abs, err := filepath.Abs(originalPath)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(projectRoot, abs)
	if err != nil {
		return "", err
	}
	return filepath.Join(projectRoot, ".nexus", "build", deployment, rel), nil
}
