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

// detectFrontendDir scans the Go files in pkgDir for a call to
// nexus.ServeFrontend (or unqualified ServeFrontend, when the user
// dot-imported the package) and returns the parent of the second
// argument's string literal — the conventional frontend project
// root for an embed-style mount:
//
//	//go:embed all:web/dist
//	var distFS embed.FS
//	nexus.ServeFrontend(distFS, "web/dist")  →  "web"
//
// Best-effort: returns "" when the call isn't present, the second
// arg isn't a plain string literal (constant / computed path), or
// the parser can't read the file. nexus dev falls through to the
// no-frontend path when this returns empty, so an unparseable
// source file never breaks the dev loop.
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
		root := serveFrontendRoot(f)
		if root == "" {
			continue
		}
		// Derive the frontend project dir from the embed root.
		// Convention: nexus.ServeFrontend(distFS, "web/dist") means
		// the watcher should run inside "web". Strip the trailing
		// component; if the path is single-segment, fall back to
		// the path itself.
		dir := filepath.Dir(root)
		if dir == "." || dir == "" {
			return root
		}
		return dir
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