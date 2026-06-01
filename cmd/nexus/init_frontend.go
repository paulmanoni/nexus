package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
)

// runInitFrontend adds the frontend pipeline scaffolding to an
// existing Go project at target: islands.src/main + App, islands/
// index.html, and a patched main.go that wires the embed + the
// ServeFrontend call. Used by `nexus init --frontend=vue|react`.
//
// Refuses to clobber islands.src/ or to re-patch a main.go that
// already references islandsFS, unless --force is supplied.
//
// The main.go patch is AST-based — we parse the file with
// go/parser, insert the missing import + embed decl + ServeFrontend
// argument inside the existing nexus.Run call, then write the
// reformatted source back. Robust against whitespace + comment
// variations; fails clearly when the file doesn't follow the
// conventional nexus shape (no nexus.Run call to locate).
func runInitFrontend(target, frontend string, force bool, stdout io.Writer) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: %s: %w", abs, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("nexus init --frontend: %s is not a directory", abs)
	}

	mainGoPath := filepath.Join(abs, "main.go")
	if _, err := os.Stat(mainGoPath); err != nil {
		return fmt.Errorf("nexus init --frontend: no main.go at %s — run nexus init in a project that already has a Go entry point", mainGoPath)
	}

	islandsSrc := filepath.Join(abs, "islands.src")
	if !force {
		if _, err := os.Stat(islandsSrc); err == nil {
			return fmt.Errorf("nexus init --frontend: %s already exists — pass --force to overwrite", islandsSrc)
		}
	}

	// 1. Generate the source files. Reuse the same templates the
	//    scaffolder uses for `nexus new --frontend=...` so the two
	//    code paths can never drift.
	opts := scaffoldOpts{
		Dir:      abs,
		Name:     filepath.Base(abs),
		Frontend: frontend,
	}
	files, err := renderFrontendOnly(opts)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: render templates: %w", err)
	}
	for path, body := range files {
		full := filepath.Join(abs, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
	}

	// 2. Patch main.go to wire the embed + ServeFrontend call.
	patched, err := patchMainGoForFrontend(mainGoPath)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: patch main.go: %w", err)
	}
	if patched {
		fmt.Fprintf(stdout, "patched main.go\n")
	} else {
		fmt.Fprintln(stdout, "main.go already wires islandsFS — skipped")
	}

	// 3. Next steps. Don't auto-run network ops here; the user
	//    decides whether to pull deps now or commit the scaffold
	//    first.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Next:")
	switch frontend {
	case "vue":
		fmt.Fprintln(stdout, "  nexus add vue           # one-time: pull vue into ~/.nexus/cache")
	case "react":
		fmt.Fprintln(stdout, "  nexus add react react-dom")
	}
	fmt.Fprintln(stdout, "  nexus types             # editor IntelliSense from nexus.lock — no npm (optional)")
	fmt.Fprintln(stdout, "  nexus dev               # Go + frontend auto-rebuild")
	if frontend == "vue" {
		fmt.Fprintln(stdout, "                          # Vue SFC compile needs the cgo+vue install:")
		fmt.Fprintln(stdout, "                          #   CGO_ENABLED=1 go install -tags vue github.com/paulmanoni/nexus/cmd/nexus@latest")
	}
	return nil
}

// renderFrontendOnly returns just the islands-side files from the
// scaffold template set, with no go.mod / main.go / module.go.
// Mirrors the HasFrontend block inside renderTemplates so the two
// stay in sync.
func renderFrontendOnly(opts scaffoldOpts) (map[string]string, error) {
	out := map[string]string{}
	add := func(path, tpl string) error {
		body, err := renderTemplate(path, tpl, opts)
		if err != nil {
			return fmt.Errorf("render %s: %w", path, err)
		}
		out[path] = body
		return nil
	}
	if err := add("islands/index.html", tmplIndexHTMLTpl); err != nil {
		return nil, err
	}
	if err := add("tsconfig.json", tmplTSConfigForIDE); err != nil {
		return nil, err
	}
	if err := add("nexus-shims.d.ts", tmplShimsDTS); err != nil {
		return nil, err
	}
	switch opts.Frontend {
	case "vue":
		if err := add("package.json", tmplPackageJSONNexusVue); err != nil {
			return nil, err
		}
		if err := add("islands.src/main.ts", tmplMainTS); err != nil {
			return nil, err
		}
		if err := add("islands.src/App.vue", tmplAppVueTpl); err != nil {
			return nil, err
		}
	case "react":
		if err := add("package.json", tmplPackageJSONNexusReact); err != nil {
			return nil, err
		}
		if err := add("islands.src/main.tsx", tmplMainTSXTpl); err != nil {
			return nil, err
		}
		if err := add("islands.src/App.tsx", tmplAppTSXTpl); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown frontend %q", opts.Frontend)
	}
	return out, nil
}

// patchMainGoForFrontend reads main.go, parses it, and modifies it
// in-place to add the three pieces a frontend-enabled main needs:
//
//  1. "embed" in the import list
//  2. //go:embed all:islands + var islandsFS embed.FS at file scope
//  3. nexus.ServeFrontend(islandsFS, "islands") as an extra
//     argument to the nexus.Run(...) call
//
// Returns (true, nil) when changes were written, (false, nil) when
// the file already had everything wired (idempotent). Errors when
// the file's shape can't be matched — typically when there's no
// nexus.Run call at top level.
//
// The AST work uses go/parser + go/printer so whitespace and
// comments are preserved as much as possible. The injection
// points are:
//
//   - import block: add "embed" if not present (use the existing
//     grouped or single-import form)
//   - file decls: append the embed declaration as a new GenDecl
//     block after the imports, before the func main() decl
//   - main func body: find the nexus.Run call expression, insert
//     a ServeFrontend(...) ast.CallExpr in its arg list after the
//     Config literal (the first positional arg)
func patchMainGoForFrontend(path string) (bool, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("parse: %w", err)
	}

	added := false

	// Step 1: ensure "embed" import is present.
	if !hasImport(file, "embed") {
		ensureImport(file, "embed")
		added = true
	}

	// Step 2: ensure the islandsFS var + //go:embed decl exists.
	if !hasVarDecl(file, "islandsFS") {
		appendEmbedDecl(file)
		added = true
	}

	// Step 3: ensure nexus.ServeFrontend is one of nexus.Run's args.
	patched, err := ensureServeFrontendArg(file)
	if err != nil {
		return false, fmt.Errorf("locate nexus.Run: %w", err)
	}
	if patched {
		added = true
	}

	if !added {
		return false, nil
	}

	// Write the patched AST back through go/format so the output
	// is gofmt-clean (proper tab indentation, import grouping
	// sorted, etc.). go/printer's raw output leaves the original
	// alignment which can look ragged after we inserted bytes.
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, file); err != nil {
		return false, fmt.Errorf("format: %w", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return false, fmt.Errorf("write: %w", err)
	}
	return true, nil
}

// hasImport reports whether file already imports the given package
// path (matching by Path.Value, including quotes).
func hasImport(file *ast.File, pkg string) bool {
	want := `"` + pkg + `"`
	for _, imp := range file.Imports {
		if imp.Path.Value == want {
			return true
		}
	}
	return false
}

// ensureImport adds pkg to the file's import block. Uses the
// existing GenDecl form (grouped if there is one; otherwise
// creates a new single import). Imports are added at the END of
// the existing block — gofmt will reorder later if the user runs
// it; not our concern here.
func ensureImport(file *ast.File, pkg string) {
	newSpec := &ast.ImportSpec{
		Path: &ast.BasicLit{Kind: token.STRING, Value: `"` + pkg + `"`},
	}
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.IMPORT {
			continue
		}
		gd.Specs = append(gd.Specs, newSpec)
		gd.Lparen = token.Pos(1) // force grouped form even if it was single before
		return
	}
	// No existing import block — synthesize one.
	gd := &ast.GenDecl{
		Tok:    token.IMPORT,
		Lparen: token.Pos(1),
		Specs:  []ast.Spec{newSpec},
	}
	file.Decls = append([]ast.Decl{gd}, file.Decls...)
}

// hasVarDecl reports whether file declares a top-level var with
// the given name.
func hasVarDecl(file *ast.File, name string) bool {
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, n := range vs.Names {
				if n.Name == name {
					return true
				}
			}
		}
	}
	return false
}

// appendEmbedDecl adds the //go:embed all:islands + var islandsFS
// embed.FS pair to the file. Inserted after the import block so
// the embed directive lands at file scope (Go requires the
// directive to be on a top-level var declaration).
func appendEmbedDecl(file *ast.File) {
	// Build:
	//   //go:embed all:islands
	//   var islandsFS embed.FS
	spec := &ast.ValueSpec{
		Names: []*ast.Ident{{Name: "islandsFS"}},
		Type: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "embed"},
			Sel: &ast.Ident{Name: "FS"},
		},
	}
	gd := &ast.GenDecl{
		Tok:   token.VAR,
		Specs: []ast.Spec{spec},
		Doc: &ast.CommentGroup{
			List: []*ast.Comment{
				{Text: "//go:embed all:islands"},
			},
		},
	}
	// Insert AFTER the last import declaration so the embed sits
	// at file scope before the first func decl.
	insertAt := 0
	for i, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			insertAt = i + 1
		}
	}
	file.Decls = append(file.Decls[:insertAt], append([]ast.Decl{gd}, file.Decls[insertAt:]...)...)
}

// ensureServeFrontendArg finds the nexus.Run(...) call expression
// inside func main and inserts nexus.ServeFrontend(islandsFS,
// "islands") as a new argument if one isn't already present.
//
// Returns (true, nil) when an arg was added; (false, nil) when the
// call already had ServeFrontend; (false, err) when no nexus.Run
// call was found (the file's shape doesn't match what we know how
// to patch).
//
// We don't try to disambiguate multiple nexus.Run calls — the
// first one wins. Real-world main.go's only have one anyway.
func ensureServeFrontendArg(file *ast.File) (bool, error) {
	var found *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := ce.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "nexus" {
			return true
		}
		if sel.Sel.Name != "Run" {
			return true
		}
		found = ce
		return false
	})
	if found == nil {
		return false, fmt.Errorf("no nexus.Run(...) call in file — main.go's shape doesn't match what we know how to patch")
	}

	// Check whether one of the existing args is already
	// nexus.ServeFrontend(...).
	for _, arg := range found.Args {
		if call, ok := arg.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if ident, ok := sel.X.(*ast.Ident); ok &&
					ident.Name == "nexus" && sel.Sel.Name == "ServeFrontend" {
					return false, nil
				}
			}
		}
	}

	// Build:  nexus.ServeFrontend(islandsFS, "islands")
	newArg := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "nexus"},
			Sel: &ast.Ident{Name: "ServeFrontend"},
		},
		Args: []ast.Expr{
			&ast.Ident{Name: "islandsFS"},
			&ast.BasicLit{Kind: token.STRING, Value: `"islands"`},
		},
	}

	// Insert AFTER the Config literal (typically args[0]) so the
	// new arg sits between Config and any module options. Falls
	// back to appending when args is empty.
	if len(found.Args) == 0 {
		found.Args = []ast.Expr{newArg}
	} else {
		insertAt := 1
		if insertAt > len(found.Args) {
			insertAt = len(found.Args)
		}
		found.Args = append(found.Args[:insertAt], append([]ast.Expr{newArg}, found.Args[insertAt:]...)...)
	}
	return true, nil
}
