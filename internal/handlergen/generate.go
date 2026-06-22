package handlergen

import (
	"fmt"
	"path/filepath"
	"sort"
)

// Site is one annotation plus the location context needed to route it to the
// right generated file: one file is emitted per package directory. Phase 4's
// CLI maps a deco scan Hit → Site (Hit.Pos.Filename's dir, Pkg, Func, Keyword,
// Args, Pos.Line); keeping Site deco-free lets the grouping logic be tested
// without the scanner.
type Site struct {
	Dir     string // directory of the source file (one generated file per dir/package)
	Pkg     string // package name in that dir
	Func    string
	Keyword string
	Args    []string
	Line    int
	Imports []string // import lines a //@use expression needs (resolved by the caller)
}

// Result is one generated file: the path to write and its formatted content.
type Result struct {
	Path    string
	Content []byte
}

// Generate groups sites by directory and emits one `outName` file per package
// that has at least one primary registration. Results are sorted by Path for
// determinism; a directory whose annotations produce no registration is
// skipped (no empty file written). It errors if a directory mixes package
// names (a scan invariant violation).
func Generate(sites []Site, outName string) ([]Result, error) {
	byDir := map[string][]Site{}
	pkgOf := map[string]string{}
	var dirs []string
	for _, s := range sites {
		if _, seen := byDir[s.Dir]; !seen {
			dirs = append(dirs, s.Dir)
		}
		byDir[s.Dir] = append(byDir[s.Dir], s)
		if p, ok := pkgOf[s.Dir]; ok && p != s.Pkg {
			return nil, fmt.Errorf("handlergen: directory %s has conflicting packages %q and %q", s.Dir, p, s.Pkg)
		}
		pkgOf[s.Dir] = s.Pkg
	}
	sort.Strings(dirs)

	var results []Result
	for _, dir := range dirs {
		group := byDir[dir]
		anns := make([]Annotation, 0, len(group))
		for _, s := range group {
			anns = append(anns, Annotation{Func: s.Func, Keyword: s.Keyword, Args: s.Args, Line: s.Line, Imports: s.Imports})
		}
		content, err := Emit(Config{Package: pkgOf[dir]}, anns)
		if err != nil {
			return nil, fmt.Errorf("handlergen: %s: %w", dir, err)
		}
		if content == nil {
			continue // no primary registrations in this package
		}
		results = append(results, Result{Path: filepath.Join(dir, outName), Content: content})
	}
	return results, nil
}
