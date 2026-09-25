package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/paulmanoni/nexus/client"
)

// runInitFrontend adds a Vite frontend to an existing Go project at
// target: the web/ project (package.json, vite.config.ts with
// nexus-vite-plugin, tsconfig.json, index.html, src entry, the committed
// web/dist/index.html stub, web/sdk/nexus-vite-plugin.{js,d.ts}) and a
// patched main.go that embeds web/dist and passes it to ServeFrontend.
// Used by `nexus init --frontend=vue|react`.
//
// An existing web/ is refused unless force is set. With force the project
// files (package.json, vite.config.ts, tsconfig.json, web/sdk) are
// rewritten, while the app's own files (index.html, src/, the dist stub)
// are written only where missing — which is how a viteless-era web/ (no
// package.json) becomes a Vite project without losing its sources.
//
// The main.go patch is AST-based — we parse the file with
// go/parser, insert the missing import + embed decl + ServeFrontend
// argument inside the existing app-entry call (nexus.Run, nexus.Boot,
// or nexus.BootFrom), then write the reformatted source back. Robust
// against whitespace + comment variations; fails clearly when the
// file doesn't follow the conventional nexus shape (no entry call to
// locate).
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

	webDir := filepath.Join(abs, "web")
	if !force {
		if _, err := os.Stat(webDir); err == nil {
			return fmt.Errorf("nexus init --frontend: %s already exists — pass --force to add the Vite project files (existing sources under web/src and web/index.html are kept)", webDir)
		}
	}

	// 1. Generate the files from the same templates `nexus new
	//    --frontend=...` uses, so the two paths can never drift.
	opts := scaffoldOpts{
		Dir:      abs,
		Name:     filepath.Base(abs),
		Frontend: frontend,
	}
	files, err := renderFrontendOnly(opts)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: render templates: %w", err)
	}
	for _, path := range slices.Sorted(maps.Keys(files)) {
		full := filepath.Join(abs, path)
		if isAppSource(path) {
			if _, err := os.Stat(full); err == nil {
				fmt.Fprintf(stdout, "kept  %s (exists)\n", path)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
		fmt.Fprintf(stdout, "wrote %s\n", path)
	}
	for _, name := range []string{"viteless.config.ts", "viteless.config.js", "viteless.config.mjs", "viteless-env.d.ts"} {
		if _, err := os.Stat(filepath.Join(webDir, name)); err == nil {
			fmt.Fprintf(stdout, "note  web/%s is no longer read — move any settings into web/vite.config.ts and delete it\n", name)
		}
	}

	// 2. Patch main.go to wire the embed + ServeFrontend call.
	patched, err := patchMainGoForFrontend(mainGoPath)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: patch main.go: %w", err)
	}
	if patched {
		fmt.Fprintf(stdout, "patched main.go\n")
	} else {
		fmt.Fprintln(stdout, "main.go already wires webFS — skipped")
	}

	// 3. Next steps. Don't auto-run network ops here; nexus dev installs
	//    the dependencies on its first run.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Next:")
	fmt.Fprintln(stdout, "  nexus dev               # first run installs web/ deps (npm, Node 20+); rebuilds on save")
	fmt.Fprintln(stdout, "                          # open the URL it prints — the app's own origin, not Vite's port")
	fmt.Fprintln(stdout, "  nexus build             # vite build → web/dist, embedded in one Go binary")
	fmt.Fprintln(stdout, "  # commit web/package-lock.json and web/sdk with the rest of web/")
	return nil
}

// isAppSource reports whether a scaffolded web/ path is the app's own
// code rather than project wiring: an existing copy is never overwritten.
func isAppSource(path string) bool {
	return path == "web/index.html" || path == "web/dist/index.html" || strings.HasPrefix(path, "web/src/")
}

// renderFrontendOnly returns the web/ file set for opts — the Vite
// project `nexus new --frontend` scaffolds and `nexus init --frontend`
// adds to an existing app. Paths are relative to the project root.
func renderFrontendOnly(opts scaffoldOpts) (map[string]string, error) {
	out := map[string]string{
		// Written verbatim, not templated: vite.config.ts imports the
		// plugin, so it must exist before the Go app first writes web/sdk
		// (nexus dev and nexus build refresh it before starting Vite).
		"web/sdk/nexus-vite-plugin.js":   string(client.VitePluginJS()),
		"web/sdk/nexus-vite-plugin.d.ts": string(client.VitePluginDTS()),
	}
	add := func(path, tpl string) error {
		body, err := renderTemplate(path, tpl, opts)
		if err != nil {
			return fmt.Errorf("render %s: %w", path, err)
		}
		out[path] = body
		return nil
	}
	var entries [][2]string
	switch {
	case opts.IsInertia():
		entries = [][2]string{
			{"web/src/main.ts", tmplInertiaMainTS},
			{"web/src/Pages/Home.vue", tmplInertiaHomeVue},
		}
		if opts.IsInertiaSSR() {
			entries = append(entries, [2]string{"web/src/ssr.ts", tmplInertiaSSRTS})
		}
	case opts.IsVue():
		entries = [][2]string{
			{"web/src/main.ts", tmplMainTS},
			{"web/src/App.vue", tmplAppVueTpl},
		}
	case opts.IsReact():
		entries = [][2]string{
			{"web/src/main.tsx", tmplMainTSXTpl},
			{"web/src/App.tsx", tmplAppTSXTpl},
		}
	default:
		return nil, fmt.Errorf("unknown frontend %q", opts.Frontend)
	}
	entries = append(entries,
		[2]string{"web/package.json", tmplPackageJSON},
		[2]string{"web/vite.config.ts", tmplViteConfig},
		[2]string{"web/tsconfig.json", tmplViteTSConfig},
		[2]string{"web/index.html", tmplViteIndexHTML},
		[2]string{"web/dist/index.html", tmplViteDistStub},
	)
	for _, e := range entries {
		if err := add(e[0], e[1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// patchMainGoForFrontend reads main.go, parses it, and modifies it
// in-place to add the three pieces a frontend-enabled main needs:
//
//  1. "embed" in the import list
//  2. //go:embed all:web/dist + var webFS embed.FS at file scope
//  3. nexus.ServeFrontend(webFS, "web/dist") as an extra
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

	// Step 2: ensure the webFS var + //go:embed decl exists.
	if !hasVarDecl(file, "webFS") {
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

// appendEmbedDecl adds the //go:embed all:web/dist + var webFS
// embed.FS pair to the file. Inserted after the import block so
// the embed directive lands at file scope (Go requires the
// directive to be on a top-level var declaration).
func appendEmbedDecl(file *ast.File) {
	// Build:
	//   //go:embed all:web/dist
	//   var webFS embed.FS
	spec := &ast.ValueSpec{
		Names: []*ast.Ident{{Name: "webFS"}},
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
				{Text: "//go:embed all:web/dist"},
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

// ensureServeFrontendArg finds the app-entry call expression inside
// func main — nexus.Run(...), nexus.Boot(...), or nexus.BootFrom(...)
// — and inserts nexus.ServeFrontend(webFS, "web/dist") as a new
// argument if one isn't already present. ServeFrontend is an Option,
// so it slots in as just another trailing argument in every form.
//
// Returns (true, nil) when an arg was added; (false, nil) when the
// call already had ServeFrontend; (false, err) when no entry call
// was found (the file's shape doesn't match what we know how to patch).
//
// We don't try to disambiguate multiple entry calls — the first one
// wins. Real-world main.go's only have one anyway.
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
		switch sel.Sel.Name {
		case "Run", "Boot", "BootFrom":
		default:
			return true
		}
		found = ce
		return false
	})
	if found == nil {
		return false, fmt.Errorf("no nexus.Run/Boot(...) call in file — main.go's shape doesn't match what we know how to patch")
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

	// Build:  nexus.ServeFrontend(webFS, "web/dist")
	newArg := &ast.CallExpr{
		Fun: &ast.SelectorExpr{
			X:   &ast.Ident{Name: "nexus"},
			Sel: &ast.Ident{Name: "ServeFrontend"},
		},
		Args: []ast.Expr{
			&ast.Ident{Name: "webFS"},
			&ast.BasicLit{Kind: token.STRING, Value: `"web/dist"`},
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
