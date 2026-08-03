package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// detectFrontendDir scans the Go files in pkgDir for one of two
// frontend-mount call shapes and returns the conventional frontend
// project root:
//
//	nexus.ServeFrontend(distFS, "web/dist")              → "web"
//	frontend.Plugin(frontend.Config{Root: "web", ...})   → "web"
//
// The legacy ServeFrontend form is checked first to preserve the
// pre-extension behavior. The frontend.Plugin form is the new
// canonical shape — its Root field directly names the frontend
// project dir, no parent-stripping needed.
//
// Best-effort: returns "" when neither call is present, the relevant
// argument isn't a plain string literal (constant / computed path),
// or the parser can't read the file. nexus dev falls through to the
// no-frontend path when this returns empty, so an unparseable
// source file never breaks the dev loop.
// detectServeFrontendRoot returns the embed root a ServeFrontend call names
// literally — "web/dist" for nexus.ServeFrontend(distFS, "web/dist") — or ""
// when there's no such call or the argument isn't a string literal.
//
// detectFrontendDir strips the trailing component to get the project dir; this
// keeps the embed root itself, which is what the dev loop stubs out of the
// build (see distStubReplacements). Deliberately does NOT cover the
// frontend.Plugin shape: that config names a project dir, not an embed root,
// so there's nothing to match embedded files against.
func detectServeFrontendRoot(pkgDir string) string {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") || strings.HasPrefix(e.Name(), "zz_") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(pkgDir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		if root := serveFrontendRoot(f); root != "" {
			return root
		}
	}
	return ""
}

func detectFrontendDir(pkgDir string) string {
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		// Skip generated overlays + tests so a stale fixture doesn't
		// influence the live dev loop.
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		if strings.HasPrefix(e.Name(), "zz_") {
			continue
		}
		path := filepath.Join(pkgDir, e.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		if root := serveFrontendRoot(f); root != "" {
			// ServeFrontend's second arg is "<dir>/dist" by
			// convention — strip the trailing component to get the
			// project root. Single-segment paths (already at the
			// project root) fall back to themselves.
			dir := filepath.Dir(root)
			if dir == "." || dir == "" {
				return root
			}
			return dir
		}
		if root := frontendPluginRoot(f); root != "" {
			// frontend.Config.Root names the project dir literally —
			// no convention stripping. Pass through as-is.
			return root
		}
	}
	return ""
}

// serveFrontendRoot walks f for the second argument of any
// ServeFrontend call expression and returns its string-literal
// value — or "" when no such call exists, or the second arg is
// non-literal.
func serveFrontendRoot(f *ast.File) string {
	var found string
	ast.Inspect(f, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isServeFrontendCall(call.Fun) {
			return true
		}
		if len(call.Args) < 2 {
			return false
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil {
			return false
		}
		found = v
		return false
	})
	return found
}

// isServeFrontendCall recognizes ServeFrontend, nexus.ServeFrontend,
// and dot-imported ServeFrontend selectors. We don't try to verify
// the package — anyone naming a helper ServeFrontend in their main
// package would be picked up too, which is fine: the resulting
// auto-spawn just tries to watch a real directory and bails if the
// path doesn't exist.
func isServeFrontendCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel != nil && f.Sel.Name == "ServeFrontend"
	case *ast.Ident:
		return f.Name == "ServeFrontend"
	}
	return false
}

// frontendPluginRoot walks f for a call shaped like
// frontend.Plugin(frontend.Config{Root: "web", ...}) and returns the
// Root field's string-literal value. Returns "" when the call isn't
// present, the first arg isn't a composite literal, or Root isn't
// set to a plain string. Constant references and computed paths are
// not resolved — the user falls back to passing --frontend explicitly.
//
// Recognizes `frontend.Plugin(...)`, dot-imported `Plugin(...)`, and
// any selector whose method name is Plugin (same loose-match policy
// as isServeFrontendCall — wrong-package false-positives surface as
// "no such dir" at watch time, not as silent miswatches).
func frontendPluginRoot(f *ast.File) string {
	var found string
	ast.Inspect(f, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isFrontendPluginCall(call.Fun) {
			return true
		}
		if len(call.Args) < 1 {
			return false
		}
		// Plugin(Config{...}) — the first arg is the composite literal.
		lit, ok := call.Args[0].(*ast.CompositeLit)
		if !ok {
			return false
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Root" {
				continue
			}
			str, ok := kv.Value.(*ast.BasicLit)
			if !ok || str.Kind != token.STRING {
				return false
			}
			v, err := strconv.Unquote(str.Value)
			if err != nil {
				return false
			}
			found = v
			return false
		}
		return false
	})
	return found
}

// isFrontendPluginCall recognizes frontend.Plugin, the dot-imported
// bare Plugin form, and any selector whose method name is Plugin.
// The loose match means a hand-written helper named Plugin would
// also match — same pragmatic choice as isServeFrontendCall. Wrong
// matches surface as missing-dir errors at watcher start, not as
// silent miswatches.
func isFrontendPluginCall(fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.SelectorExpr:
		return f.Sel != nil && f.Sel.Name == "Plugin"
	case *ast.Ident:
		return f.Name == "Plugin"
	}
	return false
}
