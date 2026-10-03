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

	// Recv and Method are set when the annotated function is a method: Recv
	// is the receiver's type name, Method the method's, and Func the method
	// expression the generated code calls, e.g. "(*UsersController).Show".
	Recv, Method string

	// TypeLevel marks a directive on a type's doc comment — //@controller and
	// its //@auth//@session//@use modifiers. Func is then the type's name.
	TypeLevel bool

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
	declsByDir, known, sites, err := collectRouterDecls(sites)
	if err != nil {
		return nil, err
	}

	byDir := map[string][]Site{}
	pkgOf := map[string]string{}
	var dirs []string
	note := func(s Site) {
		if _, seen := byDir[s.Dir]; !seen {
			dirs = append(dirs, s.Dir)
		}
	}
	for _, s := range sites {
		note(s)
		byDir[s.Dir] = append(byDir[s.Dir], s)
		if p, ok := pkgOf[s.Dir]; ok && p != s.Pkg {
			return nil, fmt.Errorf("handlergen: directory %s has conflicting packages %q and %q", s.Dir, p, s.Pkg)
		}
		pkgOf[s.Dir] = s.Pkg
	}
	// A package holding only //@router declarations still emits a file.
	for dir, decls := range declsByDir {
		if _, seen := byDir[dir]; !seen {
			dirs = append(dirs, dir)
			pkgOf[dir] = decls[0].pkg
		}
	}
	sort.Strings(dirs)

	var results []Result
	for _, dir := range dirs {
		group := byDir[dir]
		cfg, anns, err := splitPackageDirectives(Config{Package: pkgOf[dir]}, group)
		if err != nil {
			return nil, err
		}
		for _, d := range declsByDir[dir] {
			cfg.RouterDecls = append(cfg.RouterDecls, d.RouterDecl)
			if d.autoJoin {
				if cfg.Module != "" || cfg.Path != "" || cfg.RoutePrefix != "" {
					return nil, d.site.errf("//@router %s (package-named) already groups and prefixes this package — drop the //@module///@path///@routeprefix directives", d.Name)
				}
				cfg.AutoRouter = d.Name
			}
		}
		cfg.KnownRouters = known
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

// declWithPos is a router declaration plus where it came from.
type declWithPos struct {
	RouterDecl
	pkg  string
	site Site
	// autoJoin marks the package-named form (//@router <prefix> …, no name):
	// the router takes the package's name and every op in the declaring
	// package registers on it without needing //@on.
	autoJoin bool
}

// collectRouterDecls peels //@router declarations out of the site list —
// they are scan-wide, not per-package: a router declared in one package can
// be joined (//@on) from any other. Validates placement, argument shape,
// duplicate and conflicting names, unknown parents and parent cycles, all
// with positioned errors.
func collectRouterDecls(sites []Site) (byDir map[string][]declWithPos, known map[string]bool, rest []Site, err error) {
	byDir = map[string][]declWithPos{}
	known = map[string]bool{}
	decls := map[string]declWithPos{}
	rest = make([]Site, 0, len(sites))
	for _, s := range sites {
		if s.Keyword != "router" {
			rest = append(rest, s)
			continue
		}
		a := Annotation{Func: s.Func, Keyword: s.Keyword, Args: s.Args, File: s.File, Line: s.Line}
		if !s.PackageLevel {
			return nil, nil, nil, a.errf("//@router is package-level — put it on the package doc comment, above `package %s`", s.Pkg)
		}
		d, err := parseRouterDecl(a, s.Pkg)
		if err != nil {
			return nil, nil, nil, err
		}
		if prev, dup := decls[d.Name]; dup {
			if prev.RouterDecl == d.RouterDecl {
				continue // the same declaration repeated is harmless
			}
			return nil, nil, nil, a.errf("//@router %s conflicts with its declaration at %s:%d — declare a router once",
				d.Name, prev.site.File, prev.site.Line)
		}
		dw := declWithPos{RouterDecl: d.RouterDecl, pkg: s.Pkg, site: s, autoJoin: d.autoJoin}
		decls[d.Name] = dw
		byDir[s.Dir] = append(byDir[s.Dir], dw)
		known[d.Name] = true
	}
	// Parents must exist, and the parent chain must terminate.
	for name, d := range decls {
		if d.Parent == "" {
			continue
		}
		if _, ok := decls[d.Parent]; !ok {
			return nil, nil, nil, d.site.errf("//@router %s names unknown parent %q", name, d.Parent)
		}
		seen := map[string]bool{name: true}
		for p := d.Parent; p != ""; p = decls[p].Parent {
			if seen[p] {
				return nil, nil, nil, d.site.errf("//@router %s: parent chain forms a cycle through %q", name, p)
			}
			seen[p] = true
		}
	}
	return byDir, known, rest, nil
}

// errf positions an error at a site, like Annotation.errf.
func (s Site) errf(format string, args ...any) error {
	return Annotation{Func: s.Func, File: s.File, Line: s.Line}.errf(format, args...)
}

// parseRouterDecl parses the two //@router forms:
//
//	//@router <name> <prefix> [parent=…] [auth=…]   // explicit, cross-package
//	//@router <prefix> [parent=…] [auth=…]          // package-named: the router
//	                                                // takes the package's name and
//	                                                // the package's ops auto-join
//
// The forms are told apart by the first argument: a "/"-prefixed token is a
// prefix (package-named form), anything else is the router's name.
func parseRouterDecl(a Annotation, pkg string) (declWithPos, error) {
	if len(a.Args) < 1 {
		return declWithPos{}, a.errf("//@router needs a <prefix> (package-named) or <name> <prefix>, e.g. //@router /billing (got %v)", a.Args)
	}
	var d RouterDecl
	var rest []string
	if strings.HasPrefix(a.Args[0], "/") {
		// Package-named form: the same default //@module uses.
		name := pkg
		if name == "" || name == "main" {
			name = DefaultModule
		}
		d = RouterDecl{Name: name, Prefix: a.Args[0]}
		rest = a.Args[1:]
	} else {
		if len(a.Args) < 2 {
			return declWithPos{}, a.errf("//@router %s needs a <prefix>, e.g. //@router %s /billing (got %v)", a.Args[0], a.Args[0], a.Args)
		}
		d = RouterDecl{Name: a.Args[0], Prefix: a.Args[1]}
		rest = a.Args[2:]
	}
	if d.Name == "" || strings.ContainsRune(d.Name, '=') {
		return declWithPos{}, a.errf("//@router needs a name or a /-prefix first, e.g. //@router billing /billing (got %v)", a.Args)
	}
	if !strings.HasPrefix(d.Prefix, "/") {
		return declWithPos{}, a.errf("//@router %s prefix %q must start with \"/\"", d.Name, d.Prefix)
	}
	for _, kv := range rest {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			return declWithPos{}, a.errf("//@router %s: %q is not a key=value option (parent=<name>, auth=<Required|Requires(P1,P2)>)", d.Name, kv)
		}
		switch k {
		case "parent":
			d.Parent = v
		case "auth":
			expr, err := routerAuthExpr(a, v)
			if err != nil {
				return declWithPos{}, err
			}
			d.AuthExpr = expr
		default:
			return declWithPos{}, a.errf("//@router %s: unknown option %q (parent, auth)", d.Name, k)
		}
	}
	return declWithPos{RouterDecl: d, autoJoin: strings.HasPrefix(a.Args[0], "/")}, nil
}

// routerAuthExpr renders a //@router auth= value: Required, or
// Requires(P1,P2) with bare or quoted permissions.
func routerAuthExpr(a Annotation, val string) (string, error) {
	if val == "Required" || val == "Required()" {
		return "auth.Required()", nil
	}
	if inner, ok := strings.CutPrefix(val, "Requires("); ok && strings.HasSuffix(inner, ")") {
		var quoted []string
		for _, p := range strings.Split(strings.TrimSuffix(inner, ")"), ",") {
			p = strings.Trim(strings.TrimSpace(p), `"`)
			if p == "" {
				return "", a.errf("//@router auth=%s has an empty permission", val)
			}
			quoted = append(quoted, fmt.Sprintf("%q", p))
		}
		if len(quoted) == 0 {
			return "", a.errf("//@router auth=Requires(...) needs at least one permission")
		}
		return "auth.Requires(" + strings.Join(quoted, ", ") + ")", nil
	}
	return "", a.errf("//@router auth=%s is not Required or Requires(P1,P2)", val)
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
		a := Annotation{Func: s.Func, Keyword: s.Keyword, Args: s.Args, File: s.File, Line: s.Line, Imports: s.Imports,
			Recv: s.Recv, Method: s.Method, TypeLevel: s.TypeLevel}
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
