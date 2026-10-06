// Package ormgen writes the row scanners the ORM uses instead of
// reflection: one orm_scanners_gen.go per package that declares a model
// some orm.For[T]() names. nexus dev, build and test overlay its output
// (nothing lands in the tree); the ormgen command writes it to disk for a
// go:generate build.
package ormgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/paulmanoni/nexus/orm/internal/tags"
)

// The files ormgen writes in a model's package: OutTestName for models
// declared in _test.go files.
const (
	OutName      = "orm_scanners_gen.go"
	OutTestName  = "orm_scanners_gen_test.go"
	OutXTestName = "orm_scanners_x_gen_test.go" // a package_test package
)

// Config is what Generate loads: Patterns (default ./...), and Tests to
// take the models of test files too.
type Config struct {
	Patterns []string
	Tests    bool
}

const ormPath = "github.com/paulmanoni/nexus/orm"

// Generate is the scanner files of the models the packages under dir name
// with orm.For, by path.
func Generate(dir string, c Config) (map[string][]byte, error) {
	patterns := c.Patterns
	if len(patterns) == 0 {
		var err error
		if patterns, err = callers(dir, c.Tests); err != nil || len(patterns) == 0 {
			return map[string][]byte{}, err
		}
	}
	cfg := &packages.Config{
		Dir:   dir,
		Mode:  packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Tests: c.Tests,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	models := map[*types.Named]bool{}
	var order []*types.Named
	declFile := map[*types.Named]string{}
	for _, p := range pkgs {
		for _, e := range p.Errors {
			return nil, fmt.Errorf("ormgen: %s", e)
		}
		if p.TypesInfo == nil {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				id := forIdent(n)
				if id == nil {
					return true
				}
				inst, ok := p.TypesInfo.Instances[id]
				if !ok || inst.TypeArgs.Len() != 1 {
					return true
				}
				obj := p.TypesInfo.Uses[id]
				if obj == nil || obj.Pkg() == nil || obj.Pkg().Path() != ormPath || obj.Name() != "For" {
					return true
				}
				if named, ok := inst.TypeArgs.At(0).(*types.Named); ok && !models[named] {
					models[named] = true
					order = append(order, named)
					declFile[named] = p.Fset.Position(named.Obj().Pos()).Filename
				}
				return true
			})
		}
	}
	// A package's models declared in test files go in a test file of
	// their own; with Tests, a package loads twice (with and without its
	// tests), so its models are keyed by path, not by *types.Package.
	type target struct {
		pkg  *types.Package
		path string
	}
	byTarget := map[string]*target{}
	byPath := map[string][]*types.Named{}
	seen := map[string]bool{}
	dirs := map[string]string{}
	for _, p := range pkgs {
		if len(p.GoFiles) > 0 {
			dirs[p.Types.Path()] = filepath.Dir(p.GoFiles[0])
		}
	}
	// Models declared in packages that don't call For themselves: their
	// directories, without type-checking them again.
	var more []string
	for _, n := range order {
		if path := n.Obj().Pkg().Path(); dirs[path] == "" && !slices.Contains(more, path) {
			more = append(more, path)
		}
	}
	if len(more) > 0 {
		extra, err := packages.Load(&packages.Config{Dir: dir, Mode: packages.NeedName | packages.NeedFiles | packages.NeedModule}, more...)
		if err != nil {
			return nil, err
		}
		for _, p := range extra {
			if len(p.GoFiles) > 0 && p.Module != nil && p.Module.Main {
				dirs[p.PkgPath] = filepath.Dir(p.GoFiles[0])
			}
		}
	}
	for _, n := range order {
		pkg := n.Obj().Pkg()
		dir, local := dirs[pkg.Path()]
		if !local || n.TypeParams().Len() > 0 || n.Obj().Parent() != pkg.Scope() {
			continue // declared outside the packages loaded, generic, or in a function
		}
		name := OutName
		switch {
		case strings.HasSuffix(pkg.Name(), "_test"):
			name = OutXTestName
		case strings.HasSuffix(declFile[n], "_test.go"):
			name = OutTestName
		}
		path := filepath.Join(dir, name)
		key := path + "\x00" + n.Obj().Name()
		if seen[key] {
			continue
		}
		seen[key] = true
		if byTarget[path] == nil {
			byTarget[path] = &target{pkg: pkg, path: path}
		}
		byPath[path] = append(byPath[path], n)
	}
	out := map[string][]byte{}
	for path, t := range byTarget {
		src, err := file(t.pkg, byPath[path])
		if err != nil {
			return nil, err
		}
		if src != nil {
			out[path] = src
		}
	}
	return out, nil
}

// callers is the packages under dir whose source says orm.For[, as
// patterns: only they are type-checked, so a save in the dev loop stays
// cheap. Nested modules, vendor, testdata, node_modules and hidden
// directories are left out.
func callers(dir string, tests bool) ([]string, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "vendor" || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || (!tests && strings.HasSuffix(path, "_test.go")) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(src, []byte("orm.For[")) {
			return nil
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		pattern := "./" + filepath.ToSlash(rel)
		if !slices.Contains(out, pattern) {
			out = append(out, pattern)
		}
		return nil
	})
	return out, err
}

// forIdent is the For of a call orm.For[T](…), else nil.
func forIdent(n ast.Node) *ast.Ident {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return nil
	}
	ix, ok := call.Fun.(*ast.IndexExpr)
	if !ok {
		return nil
	}
	switch f := ix.X.(type) {
	case *ast.SelectorExpr:
		return f.Sel
	case *ast.Ident:
		return f
	}
	return nil
}

// column is a field the generated scanner reads.
type column struct {
	name     string // column
	path     []string
	ptrs     []string // the pointer embeds on the way, as selectors to allocate
	typ      types.Type
	depth    int
	goName   string
	computed bool
}

// columns is a model's columns as the ORM's runtime sees them, or ok
// false when the runtime would refuse the model.
func columns(st *types.Struct) ([]column, bool) {
	var out []column
	if !collect(st, nil, nil, "", &out) {
		return nil, false
	}
	return out, len(out) > 0
}

func collect(st *types.Struct, path, ptrs []string, prefix string, out *[]column) bool {
	for i := range st.NumFields() {
		f := st.Field(i)
		tag := tags.Parse(f.Name(), reflect.StructTag(st.Tag(i)), isTime(f.Type()))
		if tag.Skip {
			continue
		}
		ft := f.Type()
		base := ft
		isPtr := false
		if p, ok := ft.(*types.Pointer); ok {
			base, isPtr = p.Elem(), true
		}
		sub, isStruct := base.Underlying().(*types.Struct)
		here := append(append([]string(nil), path...), f.Name())
		if f.Anonymous() && isStruct && !isValue(ft) {
			if isPtr && !f.Exported() {
				return false
			}
			nextPtrs := ptrs
			if isPtr {
				nextPtrs = append(append([]string(nil), ptrs...), strings.Join(here, "."))
			}
			if !collect(sub, here, nextPtrs, prefix, out) {
				return false
			}
			continue
		}
		if !f.Exported() {
			continue
		}
		if tag.Embedded && isStruct {
			nextPtrs := ptrs
			if isPtr {
				nextPtrs = append(append([]string(nil), ptrs...), strings.Join(here, "."))
			}
			if !collect(sub, here, nextPtrs, prefix+tag.Prefix, out) {
				return false
			}
			continue
		}
		if !isValue(ft) {
			continue
		}
		col := tag.Column
		if col == "" {
			col = tags.Snake(f.Name())
		}
		c := column{name: prefix + col, path: here, ptrs: ptrs, typ: ft, depth: len(here), goName: f.Name(), computed: tag.Computed}
		if c.computed {
			continue
		}
		// An outer field shadows an embedded one of the same name, as in
		// the runtime.
		key := strings.ToLower(f.Name())
		replaced := false
		for j, old := range *out {
			if strings.ToLower(old.goName) == key {
				if old.depth <= c.depth {
					replaced = true
					break
				}
				*out = append((*out)[:j], (*out)[j+1:]...)
				break
			}
		}
		if !replaced {
			*out = append(*out, c)
		}
	}
	return true
}

func isTime(t types.Type) bool {
	n, ok := t.(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == "time" && n.Obj().Name() == "Time"
}

// hasMethod is whether t's method set has name.
func hasMethod(t types.Type, name string) bool {
	return types.NewMethodSet(t).Lookup(nil, name) != nil
}

func isScanner(t types.Type) bool {
	return hasMethod(t, "Scan") || hasMethod(types.NewPointer(t), "Scan")
}

// isValue mirrors the runtime's: basic kinds, times, bytes, and types that
// scan or value themselves.
func isValue(t types.Type) bool {
	if isScanner(t) || hasMethod(t, "Value") {
		return true
	}
	if p, ok := t.(*types.Pointer); ok {
		return isValue(p.Elem())
	}
	if isTime(t) {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Info()&(types.IsBoolean|types.IsInteger|types.IsFloat|types.IsString) != 0
	case *types.Slice:
		b, ok := u.Elem().Underlying().(*types.Basic)
		return ok && b.Kind() == types.Byte
	}
	return false
}

// file is the scanners of a package's models, nil when none can be
// generated.
func file(pkg *types.Package, models []*types.Named) ([]byte, error) {
	sort.Slice(models, func(i, j int) bool { return models[i].Obj().Name() < models[j].Obj().Name() })
	imports := map[string]string{ormPath: "orm"}
	o := "orm."
	if pkg.Path() == ormPath {
		imports, o = map[string]string{}, "" // the ORM's own tests
	}
	qual := func(p *types.Package) string {
		if p == pkg {
			return ""
		}
		if name, ok := imports[p.Path()]; ok {
			return name
		}
		name := p.Name()
		for taken := true; taken; {
			taken = false
			for _, n := range imports {
				if n == name {
					name += "_"
					taken = true
				}
			}
		}
		imports[p.Path()] = name
		return name
	}
	var body bytes.Buffer
	var inits bytes.Buffer
	for _, n := range models {
		st, ok := n.Underlying().(*types.Struct)
		if !ok {
			continue
		}
		cols, ok := columns(st)
		if !ok {
			continue
		}
		name := n.Obj().Name()
		scan := "ormScan" + name
		var names []string
		for _, c := range cols {
			names = append(names, strconv.Quote(c.name))
		}
		fmt.Fprintf(&inits, "\t%sRegisterScanner[%s]([]string{%s}, func() %sRowScanner[%s] { return new(%s) })\n",
			o, name, strings.Join(names, ", "), o, name, scan)

		fmt.Fprintf(&body, "type %s struct {\n", scan)
		var dest, bind []string
		allocated := map[string]bool{}
		for i, c := range cols {
			for _, p := range c.ptrs {
				if !allocated[p] {
					allocated[p] = true
					t := embedType(st, strings.Split(p, "."))
					bind = append(bind, fmt.Sprintf("if r.%s == nil {\n\t\tr.%s = new(%s)\n\t}", p, p, types.TypeString(t, qual)))
				}
			}
			ref := "&r." + strings.Join(c.path, ".")
			cellType, ptr := cellFor(c.typ, qual)
			cellType = strings.Replace(cellType, "orm.", o, 1)
			fmt.Fprintf(&body, "\tc%d %s\n", i, cellType)
			dest = append(dest, fmt.Sprintf("&s.c%d", i))
			if ptr != "" {
				ref = "(" + ptr + ")(" + ref + ")"
			}
			bind = append(bind, fmt.Sprintf("s.c%d.P = %s", i, ref))
		}
		fmt.Fprintf(&body, "\tdest []any\n}\n\n")
		fmt.Fprintf(&body, "func (s *%s) Dest() []any {\n\tif s.dest == nil {\n\t\ts.dest = []any{%s}\n\t}\n\treturn s.dest\n}\n\n", scan, strings.Join(dest, ", "))
		fmt.Fprintf(&body, "func (s *%s) Bind(r *%s) {\n\t%s\n}\n\n", scan, name, strings.Join(bind, "\n\t"))
	}
	if inits.Len() == 0 {
		return nil, nil
	}
	var src bytes.Buffer
	fmt.Fprintf(&src, "// Code generated by ormgen. DO NOT EDIT.\n\npackage %s\n\n", pkg.Name())
	paths := make([]string, 0, len(imports))
	for p := range imports {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > 0 {
		src.WriteString("import (\n")
		for _, p := range paths {
			if imports[p] == pathTail(p) {
				fmt.Fprintf(&src, "\t%q\n", p)
			} else {
				fmt.Fprintf(&src, "\t%s %q\n", imports[p], p)
			}
		}
		src.WriteString(")\n\n")
	}
	fmt.Fprintf(&src, "func init() {\n%s}\n\n%s", inits.String(), body.String())
	out, err := format.Source(src.Bytes())
	if err != nil {
		return nil, fmt.Errorf("ormgen: %s: %w\n%s", pkg.Path(), err, src.String())
	}
	return out, nil
}

// embedType is the struct a pointer embed at path points to.
func embedType(st *types.Struct, path []string) types.Type {
	var t types.Type = st
	for _, name := range path {
		s := t.Underlying().(*types.Struct)
		for i := range s.NumFields() {
			if s.Field(i).Name() == name {
				t = s.Field(i).Type()
				break
			}
		}
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
	}
	return t
}

// cellFor is the cell a field of type t scans through, and the pointer
// type its address is converted to when the cell reads its underlying
// type (a named string reads as a string).
func cellFor(t types.Type, qual types.Qualifier) (cell, conv string) {
	if p, ok := t.(*types.Pointer); ok && !isScanner(t) {
		return "orm.PtrCell[" + types.TypeString(p.Elem(), qual) + "]", ""
	}
	if _, named := t.(*types.Named); named && !isTime(t) && !isScanner(t) && !hasMethod(t, "Value") {
		if b, ok := t.Underlying().(*types.Basic); ok {
			u := types.TypeString(b, qual)
			return "orm.Cell[" + u + "]", "*" + u
		}
	}
	return "orm.Cell[" + types.TypeString(t, qual) + "]", ""
}

func pathTail(p string) string { return p[strings.LastIndexByte(p, '/')+1:] }
