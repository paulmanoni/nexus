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
// rewritten — an existing one that differs is first saved as <file>.orig
// — while the app's own files (index.html, src/, the dist stub) are
// written only where missing. That is how a viteless-era web/ becomes a
// Vite project without losing its sources or its settings.
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
			return fmt.Errorf("nexus init --frontend: %s already exists — pass --force to add the Vite project files (web/src and web/index.html are kept; a package.json, vite.config.ts or tsconfig.json it replaces is saved as <file>.orig)", webDir)
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
		if backup, err := backupBeforeOverwrite(full, files[path], path); err != nil {
			return fmt.Errorf("nexus init --frontend: %w", err)
		} else if backup != "" {
			rel, _ := filepath.Rel(abs, backup)
			fmt.Fprintf(stdout, "saved %s → %s (yours; merge anything you need back in)\n", path, filepath.ToSlash(rel))
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

	// 2. Keep web/node_modules and web/dist out of version control, as
	//    nexus new does.
	if err := ensureFrontendGitignore(abs, stdout); err != nil {
		return fmt.Errorf("nexus init --frontend: %w", err)
	}

	// 3. Patch main.go to wire the embed + ServeFrontend call.
	patched, err := patchMainGoForFrontend(mainGoPath)
	if err != nil {
		return fmt.Errorf("nexus init --frontend: patch main.go: %w", err)
	}
	if patched {
		fmt.Fprintf(stdout, "patched main.go\n")
	} else {
		fmt.Fprintln(stdout, "main.go already wires webFS — skipped")
	}

	// 4. Next steps. Don't auto-run network ops here; nexus dev installs
	//    the dependencies on its first run.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Next:")
	fmt.Fprintln(stdout, "  nexus dev               # first run installs web/ deps (npm, Node 20+); rebuilds on save")
	fmt.Fprintln(stdout, "                          # open the URL it prints — the app's own origin, not Vite's port")
	fmt.Fprintln(stdout, "  nexus build             # vite build → web/dist, embedded in one Go binary")
	fmt.Fprintln(stdout, "  # commit web/package-lock.json and web/sdk with the rest of web/")
	return nil
}

// backupBeforeOverwrite copies an existing file at full to full+".orig"
// (".orig.1", ".orig.2", … when that is taken) before --force replaces
// it with body, and returns the copy's path. A file already equal to body
// — a re-run — or one nexus generates (web/sdk) is not copied.
//
// Why a copy and not a refusal: --force exists to move a web/ onto Vite,
// and the files it replaces (package.json, vite.config.ts, tsconfig.json)
// are the ones that must change for that; refusing them would make the
// flag a no-op for exactly the case it is for. The copy keeps the user's
// dependencies, scripts and settings one diff away, and a later --force
// never overwrites an earlier copy.
func backupBeforeOverwrite(full, body, rel string) (string, error) {
	if strings.HasPrefix(rel, "web/sdk/") {
		return "", nil
	}
	old, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if string(old) == body {
		return "", nil
	}
	backup := full + ".orig"
	for i := 1; ; i++ {
		if _, err := os.Lstat(backup); os.IsNotExist(err) {
			break
		}
		backup = fmt.Sprintf("%s.orig.%d", full, i)
	}
	if err := os.WriteFile(backup, old, 0o644); err != nil {
		return "", fmt.Errorf("back up %s: %w", full, err)
	}
	return backup, nil
}

// isAppSource reports whether a scaffolded web/ path is the app's own
// code rather than project wiring: an existing copy is never overwritten.
func isAppSource(path string) bool {
	return path == "web/index.html" || path == "web/dist/index.html" || strings.HasPrefix(path, "web/src/")
}

// ensureFrontendGitignore gives the project's .gitignore the web/ entries
// nexus new writes (gitignoreFrontend), creating the file when there is
// none. Idempotent: patterns already present (as whole lines) are not
// added again, and a complete file is left untouched. Order matters for
// the stub's re-include — "!/web/dist/index.html" only works after
// "/web/dist/*" — so when the dist rule is missing both are appended.
func ensureFrontendGitignore(root string, stdout io.Writer) error {
	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read .gitignore: %w", err)
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	const (
		modules = "/web/node_modules/"
		dist    = "/web/dist/*"
		stub    = "!/web/dist/index.html"
	)
	var add []string
	if !have[modules] {
		add = append(add, modules)
	}
	if !have[dist] {
		add = append(add, dist, stub)
	} else if !have[stub] {
		add = append(add, stub)
	}
	if len(add) == 0 {
		return nil
	}
	var block string
	if len(add) == 3 {
		block = gitignoreFrontend
	} else {
		block = "\n# Frontend (web/): dependencies and build output (the dist stub is committed).\n" + strings.Join(add, "\n") + "\n"
	}
	out := append([]byte(nil), existing...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if len(out) == 0 {
		block = strings.TrimPrefix(block, "\n")
	}
	out = append(out, block...)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	if len(existing) == 0 {
		fmt.Fprintln(stdout, "wrote .gitignore")
	} else {
		fmt.Fprintf(stdout, "updated .gitignore (+ %s)\n", strings.Join(add, " "))
	}
	return nil
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
//   - file scope: the //go:embed directive + var, spliced into the
//     formatted source after the imports (insertEmbedDecl) — an AST
//     comment without a position would be dropped by the printer
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

	// Step 2 (the webFS var + //go:embed directive) happens on the
	// formatted source below: a comment synthesised into the AST has no
	// position, and the printer drops it whenever the file has comments of
	// its own — leaving a var with no directive, an empty FS.
	needEmbed := !hasVarDecl(file, "webFS")
	if needEmbed {
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
	out := buf.Bytes()
	if needEmbed {
		if out, err = insertEmbedDecl(out); err != nil {
			return false, err
		}
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("write: %w", err)
	}
	return true, nil
}

// embedDecl is the file-scope declaration a frontend-enabled main needs.
const embedDecl = "\n\n// webFS holds the built frontend (web/dist), embedded by go build.\n//\n//go:embed all:web/dist\nvar webFS embed.FS\n"

// insertEmbedDecl splices embedDecl into src right after its last import
// declaration (after the package clause when there is none) and gofmts
// the result.
func insertEmbedDecl(src []byte) ([]byte, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", src, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse patched main.go: %w", err)
	}
	at := fset.Position(file.Name.End()).Offset
	for _, decl := range file.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.IMPORT {
			at = fset.Position(gd.End()).Offset
		}
	}
	spliced := make([]byte, 0, len(src)+len(embedDecl))
	spliced = append(spliced, src[:at]...)
	spliced = append(spliced, embedDecl...)
	spliced = append(spliced, src[at:]...)
	out, err := format.Source(spliced)
	if err != nil {
		return nil, fmt.Errorf("format: %w", err)
	}
	return out, nil
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
