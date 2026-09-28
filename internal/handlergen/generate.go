package handlergen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Site is one annotation plus the location context needed to route it to the
// right generated file: one file is emitted per package directory. Phase 4's
// CLI maps a deco scan Hit → Site (Hit.Pos.Filename's dir, Pkg, Func, Keyword,
// Args, Pos.Line); keeping Site deco-free lets the grouping logic be tested
// without the scanner.
type Site struct {
	Dir     string // directory of the source file (one generated file per dir/package)
	Pkg     string // package name in that dir
	File    string // source file, as it should appear in error positions
	Func    string
	Keyword string
	Args    []string
	Line    int
	Imports []string // import lines a //@use expression needs (resolved by the caller)

	// PackageLevel marks a directive found on the package doc comment
	// (//@module, //@path, //@routeprefix) — it configures the generated
	// module instead of registering a function.
	PackageLevel bool
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
		cfg, anns, err := splitPackageDirectives(Config{Package: pkgOf[dir]}, group)
		if err != nil {
			return nil, err
		}
		content, err := Emit(cfg, anns)
		if err != nil {
			return nil, err // already positioned at the annotation (file:line)
		}
		if content == nil {
			continue // no primary registrations in this package
		}
		results = append(results, Result{Path: filepath.Join(dir, outName), Content: content})
	}
	return results, nil
}

// splitPackageDirectives peels a package's //@module//@path//@routeprefix
// directives (package doc comment) into the Config and returns the remaining
// function annotations. It enforces scope both ways — a package directive on
// a function, or a function directive on the package doc, is a positioned
// error — and rejects conflicting duplicates across the package's files.
func splitPackageDirectives(cfg Config, group []Site) (Config, []Annotation, error) {
	type first struct {
		val  string
		site Site
	}
	seen := map[string]first{}
	anns := make([]Annotation, 0, len(group))
	for _, s := range group {
		a := Annotation{Func: s.Func, Keyword: s.Keyword, Args: s.Args, File: s.File, Line: s.Line, Imports: s.Imports}
		if !packageDirectiveKeywords[s.Keyword] {
			if s.PackageLevel {
				return cfg, nil, a.errf("//@%s is not a package-level directive — annotate a function instead", s.Keyword)
			}
			anns = append(anns, a)
			continue
		}
		if !s.PackageLevel {
			return cfg, nil, a.errf("//@%s is package-level — put it on the package doc comment, above `package %s`", s.Keyword, s.Pkg)
		}
		val, err := packageDirectiveValue(a)
		if err != nil {
			return cfg, nil, err
		}
		if prev, dup := seen[s.Keyword]; dup {
			if prev.val == val {
				continue // the same declaration repeated across files is harmless
			}
			return cfg, nil, a.errf("//@%s %s conflicts with //@%s %s at %s:%d — a package declares each once",
				s.Keyword, val, s.Keyword, prev.val, prev.site.File, prev.site.Line)
		}
		seen[s.Keyword] = first{val: val, site: s}
		switch s.Keyword {
		case "module":
			cfg.Module = val
		case "path":
			cfg.Path = val
		case "routeprefix":
			cfg.RoutePrefix = val
		}
	}
	return cfg, anns, nil
}

// packageDirectiveValue validates a package directive's single argument:
// //@module needs a name, //@path//@routeprefix a "/"-prefixed prefix.
func packageDirectiveValue(a Annotation) (string, error) {
	if len(a.Args) != 1 || a.Args[0] == "" {
		what := "a module name, e.g. //@module billing"
		if a.Keyword != "module" {
			what = fmt.Sprintf("a route prefix, e.g. //@%s /billing", a.Keyword)
		}
		return "", a.errf("//@%s needs exactly %s (got %v)", a.Keyword, what, a.Args)
	}
	val := a.Args[0]
	if a.Keyword != "module" && !strings.HasPrefix(val, "/") {
		return "", a.errf("//@%s prefix %q must start with \"/\"", a.Keyword, val)
	}
	return val, nil
}
