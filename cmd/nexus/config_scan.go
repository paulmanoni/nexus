package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// The CLI validates nexus.toml in its own process, which links nexus and its
// framework extensions but not the app. The app's own declarations — its
// config.Section tables and its extension decoders — are found in its
// source: every `config.Section[T]("name")` call declares [name] with the
// keys of T (resolved from the package's type declarations; a type the
// scanner can't see through accepts any keys), and every
// `RegisterExtensionDecoder("name", …)` registers [extensions.name].

const configImportPath = "github.com/paulmanoni/nexus/v2/config"

// scannedSections and scannedExtensions record every name this process
// declared from an app's source, so the framework-only schema can be told
// apart from them (see the schema drift test).
var (
	scannedSections   = map[string]bool{}
	scannedExtensions = map[string]bool{}
)

// projectDecls is what declareProjectConfig found.
type projectDecls struct {
	Sections   []string
	Extensions []string
}

// declareProjectConfig scans the Go module rooted at dir (non-test files
// only) and declares what it finds, skipping names this process already
// knows. Safe to call more than once per process for the same dir.
func declareProjectConfig(dir string) projectDecls {
	var out projectDecls
	fset := token.NewFileSet()
	pkgs := map[string][]*ast.File{} // package dir → files
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			if p != dir {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a nested module is another program
				}
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(src), "nexus/v2") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
		if err != nil {
			return nil
		}
		pkgs[filepath.Dir(p)] = append(pkgs[filepath.Dir(p)], f)
		return nil
	})
	for _, files := range pkgs {
		types := packageTypes(files)
		for _, f := range files {
			cfgName := importName(f, configImportPath, "config")
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				name, ok := stringLit(call.Args[0])
				if !ok {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.IndexExpr: // config.Section[T]("name")
					if isSelector(fn.X, cfgName, "Section") && !config.IsDeclared(name) && validTableName(name) {
						t := (&typeResolver{types: types, seen: map[string]bool{}}).resolve(fn.Index)
						config.DeclareSchema(name, t)
						scannedSections[name] = true
						out.Sections = append(out.Sections, name)
					}
				case *ast.SelectorExpr: // config.Section("name", T{…}) — T inferred
					if isSelector(fn, cfgName, "Section") && len(call.Args) > 1 && !config.IsDeclared(name) && validTableName(name) {
						if lit, ok := call.Args[1].(*ast.CompositeLit); ok && lit.Type != nil {
							t := (&typeResolver{types: types, seen: map[string]bool{}}).resolve(lit.Type)
							config.DeclareSchema(name, t)
							scannedSections[name] = true
							out.Sections = append(out.Sections, name)
						}
					}
					// nexus.RegisterExtensionDecoder("name", …)
					if fn.Sel.Name == "RegisterExtensionDecoder" && nexus.LookupExtensionDecoder(name) == nil {
						nexus.RegisterExtensionDecoder(name, func([]byte) ([]nexus.Option, error) { return nil, nil })
						scannedExtensions[name] = true
						out.Extensions = append(out.Extensions, name)
					}
				}
				return true
			})
		}
	}
	return out
}

func validTableName(name string) bool {
	return name != "" && !strings.ContainsAny(name, ". \t\"'[]")
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// importName is the name f refers to path by ("" when f doesn't import it).
func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return def
	}
	return ""
}

// packageTypes indexes a package's type declarations by name.
func packageTypes(files []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok && ts.TypeParams == nil {
					out[ts.Name.Name] = ts.Type
				}
			}
		}
	}
	return out
}

// typeResolver turns a type expression into a reflect.Type with the same
// TOML shape. Anything it cannot see through (another package's type, a
// generic) becomes `any`, which accepts every key below it.
type typeResolver struct {
	types map[string]ast.Expr
	seen  map[string]bool
}

var anyType = reflect.TypeFor[any]()

var basicTypes = map[string]reflect.Type{
	"string": reflect.TypeFor[string](), "bool": reflect.TypeFor[bool](),
	"int": reflect.TypeFor[int](), "int8": reflect.TypeFor[int8](), "int16": reflect.TypeFor[int16](),
	"int32": reflect.TypeFor[int32](), "int64": reflect.TypeFor[int64](),
	"uint": reflect.TypeFor[uint](), "uint8": reflect.TypeFor[uint8](), "uint16": reflect.TypeFor[uint16](),
	"uint32": reflect.TypeFor[uint32](), "uint64": reflect.TypeFor[uint64](),
	"float32": reflect.TypeFor[float32](), "float64": reflect.TypeFor[float64](),
	"byte": reflect.TypeFor[byte](), "rune": reflect.TypeFor[rune](), "any": anyType,
}

func (r *typeResolver) resolve(e ast.Expr) (t reflect.Type) {
	defer func() {
		if recover() != nil {
			t = anyType
		}
	}()
	switch x := e.(type) {
	case *ast.ParenExpr:
		return r.resolve(x.X)
	case *ast.Ident:
		if bt, ok := basicTypes[x.Name]; ok {
			return bt
		}
		def, ok := r.types[x.Name]
		if !ok || r.seen[x.Name] {
			return anyType
		}
		r.seen[x.Name] = true
		defer delete(r.seen, x.Name)
		return r.resolve(def)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == "time" && x.Sel.Name == "Duration" {
			return reflect.TypeFor[time.Duration]()
		}
		return anyType
	case *ast.StarExpr:
		return r.resolve(x.X)
	case *ast.ArrayType:
		return reflect.SliceOf(r.resolve(x.Elt)) // a length is not part of the TOML shape
	case *ast.MapType:
		return reflect.MapOf(reflect.TypeFor[string](), r.resolve(x.Value))
	case *ast.StructType:
		return r.structType(x)
	}
	return anyType
}

func (r *typeResolver) structType(st *ast.StructType) reflect.Type {
	var fields []reflect.StructField
	used := map[string]bool{}
	add := func(name string, t reflect.Type, tag reflect.StructTag) {
		if used[name] {
			return
		}
		used[name] = true
		fields = append(fields, reflect.StructField{Name: name, Type: t, Tag: tag})
	}
	for _, f := range st.Fields.List {
		var tag reflect.StructTag
		if f.Tag != nil {
			if s, err := strconv.Unquote(f.Tag.Value); err == nil {
				tag = reflect.StructTag(s)
			}
		}
		if len(f.Names) == 0 {
			// Embedded: an untagged struct's keys sit in this table.
			t := r.resolve(f.Type)
			if tag.Get("toml") == "" && t.Kind() == reflect.Struct {
				for i := range t.NumField() {
					sf := t.Field(i)
					add(sf.Name, sf.Type, sf.Tag)
				}
				continue
			}
			if name := embeddedName(f.Type); name != "" && ast.IsExported(name) {
				add(name, t, tag)
			}
			continue
		}
		t := r.resolve(f.Type)
		for _, n := range f.Names {
			if n.IsExported() {
				add(n.Name, t, tag)
			}
		}
	}
	return reflect.StructOf(fields)
}

func embeddedName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return embeddedName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	}
	return ""
}
