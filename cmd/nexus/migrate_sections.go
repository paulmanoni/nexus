package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/v2/config"
)

// migrateRoot is the tree migrateTree walks, for rules that read a file
// beside the one they rewrite.
var migrateRoot string

func isMainGo(rel string) bool { return path.Base(rel) == "main.go" }

// migrateGoSections declares, in main.go, every top-level nexus.toml
// section beside it that nothing declares — v1 read app sections with
// nexus.Get alone; v2's strict config fails boot on an undeclared one.
// Each is declared free-form, config.Section[map[string]any], so the app
// boots unchanged; a TODO asks for a typed struct.
func migrateGoSections(rel string, src []byte) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.PackageClauseOnly)
	if err != nil || strings.HasSuffix(rel, "_test.go") {
		return src, nil, nil
	}
	undeclared := undeclaredSections()
	if len(undeclared) == 0 {
		return src, nil, nil
	}
	// Each section gets one owner, since a binary may declare it only
	// once: the first package (by path) that reads it, so its own tests
	// boot too; a section nothing reads goes to main.go.
	pkg := path.Dir(rel)
	owners := sectionOwners()
	var want []string
	for _, name := range undeclared {
		owner, read := owners[name]
		if read && owner == pkg && slices.Contains(sectionsRead(src), name) ||
			!read && f.Name.Name == "main" && isMainGo(rel) {
			want = append(want, name)
		}
	}
	if sectionsDeclared[pkg] == nil {
		sectionsDeclared[pkg] = map[string]bool{}
	}
	var names []string
	for _, n := range want {
		if sectionsDeclared[pkg][n] || bytes.Contains(src, []byte("Section[map[string]any]("+strconv.Quote(n)+")")) {
			continue
		}
		sectionsDeclared[pkg][n] = true
		names = append(names, n)
	}
	if len(names) == 0 {
		return src, nil, nil
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.Write(bytes.TrimRight(src, "\n"))
	b.WriteString("\n\n// nexus.toml sections this package reads. v2 fails boot on a section\n// nothing declares; these are free-form so config.Get keeps reading them.\nvar (\n")
	var changes []migrateChange
	for _, n := range names {
		fmt.Fprintf(&b, "\t// TODO(nexus v2): give [%s] a struct: config.Section[%sConfig](%q), read with .Get()\n", n, exportName(n), n)
		fmt.Fprintf(&b, "\t_ = config.Section[map[string]any](%q)\n", n)
		changes = append(changes, migrateChange{Old: "[" + n + "] undeclared", New: "config.Section[map[string]any](" + strconv.Quote(n) + ")"})
	}
	b.WriteString(")\n")
	out := b.Bytes()
	if !bytes.Contains(out, []byte(`"github.com/paulmanoni/nexus/v2/config"`)) {
		out = addImport(out, "github.com/paulmanoni/nexus/v2/config")
	}
	return out, changes, nil
}

// sectionRead matches a config read with a literal key: config.Get[T]("shop.x").
var sectionRead = regexp.MustCompile(`\b(?:config|nexus)\.(?:Get|MustGet|Has)(?:\[[^\]]*\])?\(\s*"([A-Za-z0-9_-]+)\.`)

// sectionPrefix matches a key prefix kept in a constant: "shop."
var sectionPrefix = regexp.MustCompile(`"([A-Za-z0-9_-]+)\."`)

// sectionsDeclared records, per package dir, the sections this run declared.
var sectionsDeclared = map[string]map[string]bool{}

// owners maps each section the tree's code reads to the package dir that
// declares it; nil until sectionOwners first walks the tree.
var owners map[string]string

// sectionsRead lists the sections src reads.
func sectionsRead(src []byte) []string {
	var out []string
	for _, re := range []*regexp.Regexp{sectionRead, sectionPrefix} {
		for _, m := range re.FindAllSubmatch(src, -1) {
			if name := string(m[1]); !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// sectionOwners walks migrateRoot's non-test Go files once and gives each
// section read anywhere the first reading package dir, by path.
func sectionOwners() map[string]string {
	if owners != nil {
		return owners
	}
	owners = map[string]string{}
	_ = filepath.WalkDir(migrateRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != migrateRoot && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(migrateRoot, p)
		pkg := path.Dir(filepath.ToSlash(rel))
		for _, s := range sectionsRead(src) {
			if cur, ok := owners[s]; !ok || pkg < cur {
				owners[s] = pkg
			}
		}
		return nil
	})
	return owners
}

// undeclaredSections are the top-level tables of the project's nexus.toml
// that nothing declares, sorted.
func undeclaredSections() []string {
	raw, err := os.ReadFile(filepath.Join(migrateRoot, config.DefaultPath))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []string
	for name, v := range doc {
		if _, table := v.(map[string]any); !table {
			continue
		}
		if config.IsDeclared(name) || config.SectionOwner(name) != "" {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func exportName(s string) string {
	if s == "" {
		return s
	}
	return string(bytes.ToUpper([]byte(s[:1]))) + s[1:]
}

// addImport adds an import to a Go file's first import declaration.
func addImport(src []byte, importPath string) []byte {
	q := strconv.Quote(importPath)
	if i := bytes.Index(src, []byte("import (\n")); i >= 0 {
		at := i + len("import (\n")
		return append(append(append([]byte{}, src[:at]...), []byte("\t"+q+"\n")...), src[at:]...)
	}
	if i := bytes.Index(src, []byte("\nimport ")); i >= 0 {
		return append(append(append([]byte{}, src[:i+1]...), []byte("import "+q+"\n")...), src[i+1:]...)
	}
	return src
}
