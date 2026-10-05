package viewgen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	goparser "go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/a-h/templ/parser/v2"
)

// Plan is what generating a tree produces: files to write, and stale
// generated files to remove.
type Plan struct {
	Files  map[string][]byte // absolute path → content
	Remove []string          // generated files that should no longer exist

	// Maps holds, under Options.Editor, each *_templ.go file's source map
	// back to its .templ, keyed like Files.
	Maps map[string]*TemplMap

	// Warnings point at what keeps a live page from rendering only what
	// changed (view.Assign): fields in .go files, reads in .templ files.
	// They never fail the build.
	Warnings []*PositionError
}

// TemplMap ties a generated *_templ.go to the .templ it came from.
type TemplMap struct {
	Templ string            // absolute path of the .templ source
	Map   *parser.SourceMap // .templ positions ↔ generated Go positions (0-based)
}

// Options tunes GenerateWith for an editor.
type Options struct {
	// Sources replaces the on-disk content of .templ files, by absolute
	// path: an editor's unsaved buffers. A path not on disk adds a file.
	Sources map[string][]byte
	// Editor keeps each *_templ.go as templ's generator wrote it, not
	// gofmt'ed, and records its source map in Plan.Maps — the form a
	// language server hands gopls so positions map between the two.
	Editor bool
}

// Module compiles every package under root that has .templ files, as one
// unit, and writes the result into the tree. It returns the files it
// changed — written or removed — so a watcher can tell a no-op apart.
func Module(root string) ([]string, error) {
	changed, _, err := WriteModule(root)
	return changed, err
}

// WriteModule is Module that also returns the compiler's warnings.
func WriteModule(root string) (changed []string, warnings []*PositionError, err error) {
	plan, err := Generate(root)
	if err != nil {
		return nil, nil, err
	}
	warnings = plan.Warnings
	for _, path := range plan.paths() {
		wrote, err := writeIfChanged(path, plan.Files[path])
		if err != nil {
			return changed, warnings, err
		}
		if wrote {
			changed = append(changed, path)
		}
	}
	for _, path := range plan.Remove {
		err := os.Remove(path)
		if err == nil {
			changed = append(changed, path)
		} else if !os.IsNotExist(err) {
			return changed, warnings, err
		}
	}
	return changed, warnings, nil
}

// HasTemplates reports whether the tree under root holds a .templ file —
// whether views need generating at all.
func HasTemplates(root string) bool { return len(templDirs(root)) > 0 }

// Overlay compiles the tree under root without touching it: it writes the
// generated files under dir and returns a go build -overlay file mapping
// each one into place (and hiding stale generated files).
func Overlay(root, dir string) (string, error) {
	plan, err := Generate(root)
	if err != nil {
		return "", err
	}
	replace := map[string]string{}
	for i, path := range plan.paths() {
		tmp := filepath.Join(dir, strconv.Itoa(i)+"_"+filepath.Base(path))
		if err := os.WriteFile(tmp, plan.Files[path], 0o644); err != nil {
			return "", err
		}
		replace[path] = tmp
	}
	for _, path := range plan.Remove {
		if _, err := os.Stat(path); err == nil {
			replace[path] = ""
		}
	}
	b, err := json.MarshalIndent(struct{ Replace map[string]string }{replace}, "", "  ")
	if err != nil {
		return "", err
	}
	overlay := filepath.Join(dir, "overlay.json")
	return overlay, os.WriteFile(overlay, b, 0o644)
}

func (p *Plan) paths() []string {
	out := make([]string, 0, len(p.Files))
	for path := range p.Files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// Generate compiles every package under root that has .templ files, as one
// unit: a page in one package can render a shard in another and pass on
// its gates, and a template can Use a state struct from any package of the
// module. Each package gets its *_templ.go files and view_gen.go; a main
// package gets view_imports_gen.go, importing the registering packages in
// its directory tree so the binary links them. Nothing is written.
func Generate(root string) (*Plan, error) { return GenerateWith(root, Options{}) }

// GenerateWith is Generate reading .templ sources through opts.
func GenerateWith(root string, opts Options) (*Plan, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	modDir, modPath, err := findModule(root)
	if err != nil {
		return nil, err
	}
	gomod, _ := os.ReadFile(filepath.Join(modDir, "go.mod"))
	live := bytes.Contains(gomod, []byte(strings.TrimSuffix(ViewImport, "/view")))
	importPath := func(dir string) string {
		rel, err := filepath.Rel(modDir, dir)
		if err != nil || rel == "." {
			return modPath
		}
		return modPath + "/" + filepath.ToSlash(rel)
	}
	scanned := map[string]*Package{}
	var scan func(dir string) *Package
	lookup := func(path, typ string) []string {
		if path != modPath && !strings.HasPrefix(path, modPath+"/") {
			return nil
		}
		dir := filepath.Join(modDir, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(path, modPath), "/")))
		return scan(dir).States[typ]
	}
	scan = func(dir string) *Package {
		if p, ok := scanned[dir]; ok {
			return p
		}
		p, err := Scan(dir)
		if err != nil {
			p = &Package{}
		}
		p.ImportPath, p.Lookup, p.Live = importPath(dir), lookup, live
		scanned[dir] = p
		return p
	}

	type unit struct {
		dir     string
		pkg     *Package
		results []*Result
	}
	plan := &Plan{Files: map[string][]byte{}}
	if opts.Editor {
		plan.Maps = map[string]*TemplMap{}
	}
	var units []*unit
	var errList []error
	all := map[string]*Component{}
	dirs := templDirs(root)
	for path := range opts.Sources {
		if dir := filepath.Dir(path); strings.HasSuffix(path, ".templ") && strings.HasPrefix(dir+string(filepath.Separator), root+string(filepath.Separator)) && !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, dir := range dirs {
		u := &unit{dir: dir, pkg: scan(dir)}
		matches, _ := filepath.Glob(filepath.Join(dir, "*.templ"))
		for path := range opts.Sources {
			if filepath.Dir(path) == dir && strings.HasSuffix(path, ".templ") && !slices.Contains(matches, path) {
				matches = append(matches, path)
			}
		}
		sort.Strings(matches)
		for _, path := range matches {
			src, ok := opts.Sources[path]
			if !ok {
				if src, err = os.ReadFile(path); err != nil {
					return nil, err
				}
			}
			res, err := File(path, string(src), u.pkg)
			if err != nil {
				errList = append(errList, err)
				continue
			}
			u.results = append(u.results, res)
			for _, c := range res.Components {
				all[c.ID] = c
			}
			out := strings.TrimSuffix(path, ".templ") + "_templ.go"
			plan.Files[out] = res.Go
			if opts.Editor {
				plan.Files[out] = res.RawGo
				plan.Maps[out] = &TemplMap{Templ: path, Map: res.SourceMap}
			}
		}
		units = append(units, u)
	}
	if len(errList) > 0 {
		return nil, errors.Join(errList...)
	}
	for _, u := range units {
		pages := map[string]bool{}
		for _, r := range u.results {
			plan.Warnings = append(plan.Warnings, r.Warnings...)
			for name := range r.Pages {
				pages[name] = true
			}
		}
		plan.Warnings = append(plan.Warnings, pageWarnings(u.pkg, pages)...)
	}

	type registering struct{ dir, path string }
	var registered []registering
	for _, u := range units {
		gen, err := Registrations(u.results, u.pkg, all)
		if err != nil {
			errList = append(errList, err)
			continue
		}
		plan.Remove = append(plan.Remove, filepath.Join(u.dir, "view_twins_gen.go"))
		genFile := filepath.Join(u.dir, "view_gen.go")
		if gen == nil {
			plan.Remove = append(plan.Remove, genFile)
			continue
		}
		plan.Files[genFile] = gen
		if len(u.results) > 0 && u.results[0].Package != "main" {
			registered = append(registered, registering{u.dir, u.pkg.ImportPath})
		}
	}
	if len(errList) > 0 {
		return nil, errors.Join(errList...)
	}

	for _, dir := range mainDirs(root) {
		file := filepath.Join(dir, "view_imports_gen.go")
		var paths []string
		for _, r := range registered {
			if r.dir == dir || strings.HasPrefix(r.dir, dir+string(filepath.Separator)) {
				paths = append(paths, r.path)
			}
		}
		if len(paths) == 0 {
			plan.Remove = append(plan.Remove, file)
			continue
		}
		sort.Strings(paths)
		var b bytes.Buffer
		b.WriteString("// Code generated by nexus; DO NOT EDIT.\n\n// The packages whose pages and shards register themselves.\n\npackage main\n\nimport (\n")
		for _, p := range paths {
			fmt.Fprintf(&b, "\t_ %s\n", strconv.Quote(p))
		}
		b.WriteString(")\n")
		src, err := format.Source(b.Bytes())
		if err != nil {
			return nil, err
		}
		plan.Files[file] = src
	}
	// A generated file left where nothing generates it any more (the
	// package's templates were deleted) must go too. This runs last, once
	// every file this run generates is in the plan.
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if name := d.Name(); name != "view_gen.go" && name != "view_imports_gen.go" {
			return nil
		}
		if _, planned := plan.Files[path]; planned {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil && bytes.HasPrefix(b, []byte("// Code generated by nexus")) {
			plan.Remove = append(plan.Remove, path)
		}
		return nil
	})

	return plan, nil
}

// findModule finds the go.mod at or above dir: its directory and module path.
func findModule(dir string) (string, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for d := abs; ; d = filepath.Dir(d) {
		b, err := os.ReadFile(filepath.Join(d, "go.mod"))
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
					return d, strings.Trim(f[1], `"`), nil
				}
			}
			return "", "", fmt.Errorf("%s/go.mod has no module line", d)
		}
		if filepath.Dir(d) == d {
			return "", "", fmt.Errorf("no go.mod at or above %s", dir)
		}
	}
}

// templDirs lists the directories under root holding .templ files.
func templDirs(root string) []string {
	root, _ = filepath.Abs(root)
	seen := map[string]bool{}
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if dir := filepath.Dir(path); strings.HasSuffix(path, ".templ") && !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return nil
	})
	return dirs
}

// mainDirs lists the directories under root whose Go package is main.
func mainDirs(root string) []string {
	root, _ = filepath.Abs(root)
	var dirs []string
	seen := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.Dir(path)
		if seen[dir] || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.PackageClauseOnly)
		if err == nil && f.Name.Name == "main" {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return nil
	})
	return dirs
}
