// Package apicheck guards the public API's seams with go/types: it loads
// every non-internal package of the module from compiler export data and
// walks each exported identifier's type for names the public API must not
// mention.
package apicheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/importer"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const modulePath = "github.com/paulmanoni/nexus/v2"

// TestNoGraphQLEngineInPublicAPI keeps graphql-go behind the seam: no
// exported identifier of a public package may reach a graphql-go type —
// directly, or through the exported surface of an internal nexus type it
// hands out. The engine can then be replaced in a minor release.
func TestNoGraphQLEngineInPublicAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("loads export data for the whole module")
	}
	leaks := publicLeaks(t, []string{"github.com/graphql-go/"})
	if len(leaks) > 0 {
		t.Fatalf("%d exported identifiers mention graphql-go; move them under internal/ or use nexus types:\n  %s",
			len(leaks), strings.Join(leaks, "\n  "))
	}
}

type listedPkg struct {
	ImportPath string
	Export     string
	Name       string
}

func moduleRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func publicLeaks(t *testing.T, bad []string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-export", "-deps", "-json=ImportPath,Export,Name", "./...")
	cmd.Dir = moduleRoot()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var targets []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		exports[p.ImportPath] = p.Export
		if isPublic(p.ImportPath) && p.Name != "main" {
			targets = append(targets, p.ImportPath)
		}
	}
	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		f := exports[path]
		if f == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(f)
	})
	var leaks []string
	for _, path := range targets {
		pkg, err := imp.Import(path)
		if err != nil {
			t.Fatalf("import %s: %v", path, err)
		}
		leaks = append(leaks, scanPackage(pkg, bad)...)
	}
	sort.Strings(leaks)
	return leaks
}

func isPublic(path string) bool {
	if path != modulePath && !strings.HasPrefix(path, modulePath+"/") {
		return false
	}
	rest := strings.TrimPrefix(path, modulePath)
	return !strings.Contains(rest+"/", "/internal/") && !strings.HasPrefix(rest, "/examples/")
}

func scanPackage(pkg *types.Package, bad []string) []string {
	var leaks []string
	short := strings.TrimPrefix(strings.TrimPrefix(pkg.Path(), modulePath), "/")
	if short == "" {
		short = "nexus"
	}
	report := func(name, via string) {
		leaks = append(leaks, fmt.Sprintf("%s.%s -> %s", short, name, via))
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		if tn, ok := obj.(*types.TypeName); ok && !tn.IsAlias() {
			for _, m := range members(tn.Type()) {
				if via := reaches(m.typ, bad, map[types.Type]bool{}); via != "" {
					report(name+"."+m.name, via)
				}
			}
			if via := reaches(tn.Type().Underlying(), bad, map[types.Type]bool{}); via != "" {
				report(name, via)
			}
			continue
		}
		if via := reaches(obj.Type(), bad, map[types.Type]bool{}); via != "" {
			report(name, via)
		}
	}
	return leaks
}

type member struct {
	name string
	typ  types.Type
}

// members lists a named type's exported methods, pointer receivers
// included.
func members(t types.Type) []member {
	named, ok := t.(*types.Named)
	if !ok {
		return nil
	}
	var out []member
	ms := types.NewMethodSet(types.NewPointer(named))
	for i := 0; i < ms.Len(); i++ {
		if f := ms.At(i).Obj(); f.Exported() {
			out = append(out, member{f.Name(), f.Type()})
		}
	}
	return out
}

func forbiddenPath(path string, bad []string) bool {
	for _, b := range bad {
		if strings.HasPrefix(path, b) {
			return true
		}
	}
	return false
}

// reaches reports the first forbidden named type t refers to. It follows
// the exported surface of internal nexus types t passes through: a public
// function returning an internal type still hands the caller its methods
// and fields. Public nexus types are checked on their own.
func reaches(t types.Type, bad []string, seen map[types.Type]bool) string {
	if t == nil || seen[t] {
		return ""
	}
	seen[t] = true
	switch t := t.(type) {
	case *types.Named:
		obj := t.Obj()
		if obj.Pkg() == nil {
			return ""
		}
		path := obj.Pkg().Path()
		if forbiddenPath(path, bad) {
			return obj.Pkg().Name() + "." + obj.Name()
		}
		if targs := t.TypeArgs(); targs != nil {
			for i := 0; i < targs.Len(); i++ {
				if v := reaches(targs.At(i), bad, seen); v != "" {
					return v
				}
			}
		}
		if strings.HasPrefix(path, modulePath) && !isPublic(path) && obj.Exported() {
			for _, m := range members(t) {
				if v := reaches(m.typ, bad, seen); v != "" {
					return v
				}
			}
			return reaches(t.Underlying(), bad, seen)
		}
	case *types.Alias:
		return reaches(types.Unalias(t), bad, seen)
	case *types.Pointer:
		return reaches(t.Elem(), bad, seen)
	case *types.Slice:
		return reaches(t.Elem(), bad, seen)
	case *types.Array:
		return reaches(t.Elem(), bad, seen)
	case *types.Map:
		if v := reaches(t.Key(), bad, seen); v != "" {
			return v
		}
		return reaches(t.Elem(), bad, seen)
	case *types.Chan:
		return reaches(t.Elem(), bad, seen)
	case *types.Signature:
		if v := reaches(t.Params(), bad, seen); v != "" {
			return v
		}
		return reaches(t.Results(), bad, seen)
	case *types.Tuple:
		for i := 0; i < t.Len(); i++ {
			if v := reaches(t.At(i).Type(), bad, seen); v != "" {
				return v
			}
		}
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			if f := t.Field(i); f.Exported() || f.Embedded() {
				if v := reaches(f.Type(), bad, seen); v != "" {
					return v
				}
			}
		}
	case *types.Interface:
		for i := 0; i < t.NumMethods(); i++ {
			if m := t.Method(i); m.Exported() {
				if v := reaches(m.Type(), bad, seen); v != "" {
					return v
				}
			}
		}
		for i := 0; i < t.NumEmbeddeds(); i++ {
			if v := reaches(t.EmbeddedType(i), bad, seen); v != "" {
				return v
			}
		}
	case *types.TypeParam:
		return reaches(t.Constraint(), bad, seen)
	case *types.Union:
		for i := 0; i < t.Len(); i++ {
			if v := reaches(t.Term(i).Type(), bad, seen); v != "" {
				return v
			}
		}
	}
	return ""
}
