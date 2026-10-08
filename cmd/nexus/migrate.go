package main

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/mod/modfile"
)

// `nexus migrate v2` is the codemod from nexus v1 to v2 (docs/design/v2.md,
// Migration). It walks a project tree and runs a table of rules over every
// file a rule claims: each rule is a pure source→source rewrite that also
// reports what it changed, so the same table drives the write, the --dry-run
// summary and the tests. Rules are idempotent — a migrated tree migrates to
// itself — which makes the command safe to re-run after a partial migration
// or a merge that brought v1 code back in.
//
// Adding a rule (nexus.toml key moves, …) is one entry in migrateV2Rules:
// a name, the files it applies to, and the rewrite. A moved or renamed
// exported name is one row in migrateV2Symbols (migrate_symbols.go).

// nexusModule is the v1 root module path; v2 lives at nexusModule + "/v2".
const nexusModule = "github.com/paulmanoni/nexus"

// nexusSubmodules are the repo directories that are modules of their own.
// Each takes the /v2 suffix on its own path (…/httpx/ginrouter/v2) rather
// than moving under the root's /v2. view is NOT here: in v2 it is a package
// of the root module (…/nexus/v2/view).
var nexusSubmodules = []string{
	"cmd/nexus",
	"di/fxcontainer",
	"extension/cache/redis",
	"extension/jobs/jobsamqp",
	"extension/jobs/jobsredis",
	"httpx/ginrouter",
}

// nexusV2Version is the version go.mod requirements are rewritten to.
const nexusV2Version = "v2.0.0"

// migrateChange is one edit a rule made, for the report: the 1-based line in
// the ORIGINAL source (0 when the edit has no single line) and a short
// before → after.
type migrateChange struct {
	Line     int
	Old, New string
}

// migrateRule is one row of the codemod table.
type migrateRule struct {
	// Name labels the rule in reports ("imports", "go.mod", "annotations").
	Name string
	// Applies selects the files the rule rewrites, by slash path relative
	// to the migrated root.
	Applies func(rel string) bool
	// Apply rewrites src, returning the new source (src itself when nothing
	// changed) and the edits made.
	Apply func(rel string, src []byte) ([]byte, []migrateChange, error)
}

// migrateV2Rules is the v1 → v2 table, applied in order per file. A .go file
// any rule changed is gofmt'ed afterwards (import paths re-sort, and gofmt
// moves //nexus: directives below the doc prose).
var migrateV2Rules = []migrateRule{
	{Name: "imports", Applies: isGoSource, Apply: migrateGoImports},
	{Name: "imports", Applies: isTemplSource, Apply: migrateTemplImports},
	{Name: "symbols", Applies: isGoSource, Apply: migrateGoSymbols},
	{Name: "errors", Applies: isGoSource, Apply: migrateGoErrors},
	{Name: "tags", Applies: isGoSource, Apply: migrateGoTags},
	{Name: "go.mod", Applies: isGoMod, Apply: migrateGoMod},
	{Name: "annotations", Applies: isGoSource, Apply: migrateGoAnnotations},
	{Name: "annotations", Applies: isTemplSource, Apply: migrateTemplAnnotations},
	{Name: "nexus.toml", Applies: isNexusTOML, Apply: migrateNexusTOMLKeys},
	{Name: "assembly", Applies: isGoSource, Apply: migrateGoAssembly},
	{Name: "op names", Applies: isGoSource, Apply: migrateGoOpNames},
	{Name: "fields", Applies: isGoSource, Apply: migrateGoFields},
	{Name: "spreads", Applies: isGoSource, Apply: migrateGoSpreads},
	{Name: "sections", Applies: isGoSource, Apply: migrateGoSections},
}

func isGoSource(rel string) bool    { return strings.HasSuffix(rel, ".go") }
func isTemplSource(rel string) bool { return strings.HasSuffix(rel, ".templ") }
func isGoMod(rel string) bool       { return path.Base(rel) == "go.mod" }

// migrateFileResult is the outcome for one changed file.
type migrateFileResult struct {
	Rel     string
	Rules   []string                   // rule names that changed it, in order
	Changes map[string][]migrateChange // rule name → edits
	Out     []byte
}

func newMigrateV2Cmd(stdout, _ io.Writer) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "v2 [dir]",
		Short: "Rewrite a nexus v1 project for nexus v2",
		Long: `Rewrite a nexus v1 project tree (default: the current directory) for v2.

Rules, applied to every file under the tree (vendor, node_modules, testdata
and hidden directories are skipped):

  imports      Go and templ import paths move to /v2:
                 github.com/paulmanoni/nexus[/p]       -> github.com/paulmanoni/nexus/v2[/p]
                 github.com/paulmanoni/nexus/view[/p]  -> github.com/paulmanoni/nexus/v2/view[/p]
               the separate modules take /v2 on their own path:
                 github.com/paulmanoni/nexus/httpx/ginrouter -> …/httpx/ginrouter/v2
                 (also cmd/nexus, di/fxcontainer, extension/cache/redis,
                 extension/jobs/jobsamqp, extension/jobs/jobsredis)
               Only import specs change, never other strings.
  symbols      moved and renamed exported names, in Go files that import nexus
               (an aliased import is followed; the target package is imported,
               and an import left unused is dropped). The table is listed below.
               A name with no mechanical replacement is dropped from the option
               list it sits in, with a // TODO(nexus v2): comment above its
               statement. App.UseVolume (a method) is not rewritten.
  errors       the error model (docs/design/v2.md §2): nexus.Error(err), the
               boot option, becomes nexus.FailBoot(err); the error rows of the
               table below follow (nexus.Errors is nexus.Error, built with
               nexus.Invalid(); Field, Global, Any and First are unchanged).
  tags         the retired uri:"x" struct tag becomes path:"x" in Go files that
               import nexus (a field that already has path: drops its uri:).
  go.mod       require lines for those modules move to the new paths at
               ` + nexusV2Version + `; a github.com/paulmanoni/nexus/view requirement is
               dropped (view is part of the root module in v2); replace
               directives follow their module. Run go mod tidy afterwards.
  annotations  //@x and // @x nexus annotations in .go and .templ files become
               Go directives: //nexus:x (built-in keywords and custom
               //@pkg.Func decorators; other tools' @-annotations are left alone).
  nexus.toml   keys v1 silently ignored because they sat in the wrong table
               (a top-level environment, an addr under [runtime]) move to the
               table the strict v2 check names; typos and undeclared sections
               are left for nexus config check.
  assembly     app-assembly code with no mechanical v2 form gets a
               // TODO(nexus v2): comment naming its replacement:
               Config.Middleware.Global (nexus.Middleware), AsCRUD
               (nexus.Resource), pre-serve nexus.Invoke work (nexus.Setup).
  op names     GraphQL registrations of a NewXxx handler get nexus.Op("xxx")
               (or //nexus:use nexus.Op("xxx") when annotated), keeping the v1
               wire name now that v2 no longer strips New.
  fields       renamed struct fields in nexus composite literals:
               middleware.Middleware{Gin: …} becomes {HTTP: …}.
  spreads      nexus.MustLoadExtensions()... loses its spread (it returns one
               Option in v2).
  sections     every top-level nexus.toml table nothing declares is declared in
               main.go as config.Section[map[string]any], so strict config
               boots; a TODO asks for a typed struct.

Changed .go files are gofmt'ed. Re-running is a no-op. --dry-run prints
every change without writing.

Symbols:
` + migrateSymbolTable(migrateV2Symbols) + migrateSymbolTable(migrateV2ErrorSymbols),
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			results, err := migrateTree(root, migrateV2Rules)
			if err != nil {
				return fmt.Errorf("nexus migrate v2: %w", err)
			}
			if !dryRun {
				for _, r := range results {
					p := filepath.Join(root, filepath.FromSlash(r.Rel))
					info, err := os.Stat(p)
					if err != nil {
						return fmt.Errorf("nexus migrate v2: %w", err)
					}
					if err := os.WriteFile(p, r.Out, info.Mode().Perm()); err != nil {
						return fmt.Errorf("nexus migrate v2: write %s: %w", r.Rel, err)
					}
				}
			}
			printMigrateReport(stdout, results, dryRun)
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print every change without writing any file")
	return cmd
}

// printMigrateReport lists each changed file with its per-rule counts; a dry
// run also lists every edit.
func printMigrateReport(w io.Writer, results []migrateFileResult, dryRun bool) {
	if len(results) == 0 {
		fmt.Fprintln(w, "nothing to migrate: the tree is already on nexus v2")
		return
	}
	gomod := false
	for _, r := range results {
		parts := make([]string, 0, len(r.Rules))
		for _, name := range r.Rules {
			parts = append(parts, fmt.Sprintf("%s %d", name, len(r.Changes[name])))
		}
		fmt.Fprintf(w, "  %s  (%s)\n", r.Rel, strings.Join(parts, ", "))
		if dryRun {
			for _, name := range r.Rules {
				for _, c := range r.Changes[name] {
					at := ""
					if c.Line > 0 {
						at = fmt.Sprintf(":%d", c.Line)
					}
					fmt.Fprintf(w, "      %s%s  %s → %s\n", name, at, c.Old, orDropped(c.New))
				}
			}
		}
		if isGoMod(r.Rel) {
			gomod = true
		}
	}
	verb := "migrated"
	if dryRun {
		verb = "dry run: would migrate"
	}
	fmt.Fprintf(w, "%s %d file(s)\n", verb, len(results))
	if gomod && !dryRun {
		fmt.Fprintln(w, "next: run `go mod tidy` in each changed module")
	}
}

func orDropped(s string) string {
	if s == "" {
		return "(dropped)"
	}
	return s
}

// migrateTree runs rules over every file under root and returns the files
// that changed, sorted by path. Nothing is written.
func migrateTree(root string, rules []migrateRule) ([]migrateFileResult, error) {
	migrateRoot = root
	sectionsDeclared = map[string]map[string]bool{}
	owners = nil
	var results []migrateFileResult
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "vendor" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		var applicable []migrateRule
		for _, r := range rules {
			if r.Applies(rel) {
				applicable = append(applicable, r)
			}
		}
		if len(applicable) == 0 {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		res, err := migrateFile(rel, src, applicable)
		if err != nil {
			return err
		}
		if res != nil {
			results = append(results, *res)
		}
		return nil
	})
	sort.Slice(results, func(i, j int) bool { return results[i].Rel < results[j].Rel })
	return results, err
}

// migrateFile applies rules to one file's source; nil when nothing changed.
func migrateFile(rel string, src []byte, rules []migrateRule) (*migrateFileResult, error) {
	res := &migrateFileResult{Rel: rel, Changes: map[string][]migrateChange{}}
	out := src
	for _, r := range rules {
		next, changes, err := r.Apply(rel, out)
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", rel, r.Name, err)
		}
		if len(changes) == 0 {
			continue
		}
		if _, seen := res.Changes[r.Name]; !seen {
			res.Rules = append(res.Rules, r.Name)
		}
		res.Changes[r.Name] = append(res.Changes[r.Name], changes...)
		out = next
	}
	if len(res.Rules) == 0 {
		return nil, nil
	}
	if isGoSource(rel) {
		formatted, err := format.Source(out)
		if err != nil {
			return nil, fmt.Errorf("%s: gofmt: %w", rel, err)
		}
		out = formatted
	}
	if bytes.Equal(out, src) {
		return nil, nil
	}
	res.Out = out
	return res, nil
}

// migrateImportPath maps a v1 nexus import path to its v2 path; ok is false
// for paths outside nexus and paths already on v2.
func migrateImportPath(p string) (string, bool) {
	if p == nexusModule {
		return nexusModule + "/v2", true
	}
	rest, ok := strings.CutPrefix(p, nexusModule+"/")
	if !ok {
		return "", false
	}
	if rest == "v2" || strings.HasPrefix(rest, "v2/") {
		return "", false
	}
	for _, sub := range nexusSubmodules {
		if rest == sub || strings.HasPrefix(rest, sub+"/") {
			tail := rest[len(sub):]
			if tail == "/v2" || strings.HasPrefix(tail, "/v2/") {
				return "", false
			}
			return nexusModule + "/" + sub + "/v2" + tail, true
		}
	}
	// Every other package — view included — is in the root module.
	return nexusModule + "/v2/" + rest, true
}

// migrateGoImports rewrites the import specs of a Go file. Only the quoted
// path of each spec changes, by byte offset, so the rest of the file is
// untouched until the final gofmt.
func migrateGoImports(_ string, src []byte) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	var changes []migrateChange
	for _, spec := range f.Imports {
		old, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		nw, ok := migrateImportPath(old)
		if !ok {
			continue
		}
		edits = append(edits, edit{
			start: fset.Position(spec.Path.Pos()).Offset,
			end:   fset.Position(spec.Path.End()).Offset,
			text:  strconv.Quote(nw),
		})
		changes = append(changes, migrateChange{Line: fset.Position(spec.Path.Pos()).Line, Old: strconv.Quote(old), New: strconv.Quote(nw)})
	}
	if len(edits) == 0 {
		return src, nil, nil
	}
	var b bytes.Buffer
	last := 0
	for _, e := range edits {
		b.Write(src[last:e.start])
		b.WriteString(e.text)
		last = e.end
	}
	b.Write(src[last:])
	return b.Bytes(), changes, nil
}

// templQuoted matches a quoted path on a templ import line.
var templQuoted = regexp.MustCompile(`"[^"\n]*"`)

// migrateTemplImports rewrites the import paths of a .templ file. templ's Go
// header is not parseable as Go, so this tracks import declarations line by
// line: `import "p"`, `import name "p"` and the lines of an `import ( … )`
// block. Strings anywhere else are left alone.
func migrateTemplImports(_ string, src []byte) ([]byte, []migrateChange, error) {
	lines := strings.SplitAfter(string(src), "\n")
	var changes []migrateChange
	inBlock := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		isImport := false
		switch {
		case inBlock:
			if strings.HasPrefix(trimmed, ")") {
				inBlock = false
				continue
			}
			isImport = true
		case strings.HasPrefix(trimmed, "import ("), trimmed == "import(":
			inBlock = !strings.Contains(trimmed, ")")
			continue
		case strings.HasPrefix(trimmed, "import "):
			isImport = true
		}
		if !isImport {
			continue
		}
		lines[i] = templQuoted.ReplaceAllStringFunc(line, func(q string) string {
			old, err := strconv.Unquote(q)
			if err != nil {
				return q
			}
			nw, ok := migrateImportPath(old)
			if !ok {
				return q
			}
			changes = append(changes, migrateChange{Line: i + 1, Old: q, New: strconv.Quote(nw)})
			return strconv.Quote(nw)
		})
	}
	if len(changes) == 0 {
		return src, nil, nil
	}
	return []byte(strings.Join(lines, "")), changes, nil
}

// migrateGoMod moves require (and replace) lines for nexus modules to their
// v2 paths at nexusV2Version. A requirement on the v1 view module is dropped
// — view is a package of the v2 root module — and the root is required in
// its place when nothing else requires it. Edits happen on the parsed syntax
// tree, so comments and block layout survive.
func migrateGoMod(rel string, src []byte) ([]byte, []migrateChange, error) {
	f, err := modfile.Parse(rel, src, nil)
	if err != nil {
		return nil, nil, err
	}
	var changes []migrateChange
	required := map[string]bool{}
	for _, r := range f.Require {
		required[r.Mod.Path] = true
	}
	viewPath := nexusModule + "/view"
	rootV2 := nexusModule + "/v2"
	addRoot := false
	for _, r := range f.Require {
		old := r.Mod
		line := 0
		if r.Syntax != nil {
			line = r.Syntax.Start.Line
		}
		if old.Path == viewPath {
			if err := f.DropRequire(old.Path); err != nil {
				return nil, nil, err
			}
			changes = append(changes, migrateChange{Line: line, Old: "require " + old.Path + " " + old.Version})
			if !required[rootV2] && !required[nexusModule] {
				addRoot = true
			}
			continue
		}
		nw, ok := migrateModulePath(old.Path)
		if !ok {
			continue
		}
		if required[nw] {
			// Already required at its v2 path (a half-migrated go.mod).
			if err := f.DropRequire(old.Path); err != nil {
				return nil, nil, err
			}
			changes = append(changes, migrateChange{Line: line, Old: "require " + old.Path + " " + old.Version})
			continue
		}
		required[nw] = true
		setModTokens(r.Syntax, old.Path, nw, nexusV2Version, true)
		r.Mod.Path, r.Mod.Version = nw, nexusV2Version
		changes = append(changes, migrateChange{Line: line, Old: "require " + old.Path + " " + old.Version, New: "require " + nw + " " + nexusV2Version})
	}
	if addRoot {
		if err := f.AddRequire(rootV2, nexusV2Version); err != nil {
			return nil, nil, err
		}
		changes = append(changes, migrateChange{New: "require " + rootV2 + " " + nexusV2Version})
	}
	for _, r := range f.Replace {
		nw, ok := migrateModulePath(r.Old.Path)
		if r.Old.Path == viewPath {
			// The view module is gone; its replacement goes with it.
			// (DropReplace zeroes r, so read it first.)
			change := migrateChange{Line: r.Syntax.Start.Line, Old: "replace " + r.Old.Path}
			if err := f.DropReplace(r.Old.Path, r.Old.Version); err != nil {
				return nil, nil, err
			}
			changes = append(changes, change)
			continue
		}
		if !ok {
			continue
		}
		// The v1 version on the left (if any) cannot match a v2 module; the
		// replacement applies to every version of the new path.
		setModTokens(r.Syntax, r.Old.Path, nw, "", r.Old.Version != "")
		changes = append(changes, migrateChange{Line: r.Syntax.Start.Line, Old: "replace " + r.Old.Path, New: "replace " + nw})
		r.Old.Path, r.Old.Version = nw, ""
	}
	if len(changes) == 0 {
		return src, nil, nil
	}
	f.Cleanup()
	out := modfile.Format(f.Syntax)
	return out, changes, nil
}

// migrateModulePath maps a v1 nexus MODULE path (the root or a submodule)
// to its v2 path; other paths — packages, the retired view module — are not
// module paths go.mod can require and report ok=false.
func migrateModulePath(p string) (string, bool) {
	if p == nexusModule {
		return nexusModule + "/v2", true
	}
	for _, sub := range nexusSubmodules {
		if p == nexusModule+"/"+sub {
			return p + "/v2", true
		}
	}
	return "", false
}

// setModTokens rewrites a go.mod line's module path token in place. With
// hasVersion, the token after the path is the version: replaced with ver,
// or removed when ver is "".
func setModTokens(l *modfile.Line, oldPath, newPath, ver string, hasVersion bool) {
	if l == nil {
		return
	}
	for i, t := range l.Token {
		if t != oldPath && t != strconv.Quote(oldPath) {
			continue
		}
		l.Token[i] = newPath
		if hasVersion && i+1 < len(l.Token) && l.Token[i+1] != "=>" {
			if ver == "" {
				l.Token = append(l.Token[:i+1], l.Token[i+2:]...)
			} else {
				l.Token[i+1] = ver
			}
		}
		return
	}
}

// legacyAnnotation splits a comment line holding a nexus v1 annotation
// (//@rest GET /x, or gofmt's // @rest GET /x) into the keyword and the
// remainder of the line; ok is false for any other comment, including other
// tools' @-annotations.
func legacyAnnotation(text string) (kw, rest string, ok bool) {
	body, found := strings.CutPrefix(text, "//")
	if !found {
		return "", "", false
	}
	body = strings.TrimLeft(body, " \t")
	body, found = strings.CutPrefix(body, "@")
	if !found {
		return "", "", false
	}
	end := strings.IndexAny(body, " \t")
	if end < 0 {
		end = len(body)
	}
	kw = body[:end]
	if !legacyNexusKeyword(kw) {
		return "", "", false
	}
	return kw, body[end:], true
}

// migrateGoAnnotations rewrites v1 annotation comments of a Go file to
// //nexus: directives. Only real comments are touched (a "//@rest" inside a
// string literal is data); the gofmt that follows moves the directives below
// the doc prose.
func migrateGoAnnotations(_ string, src []byte) ([]byte, []migrateChange, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
	var b bytes.Buffer
	var changes []migrateChange
	last := 0
	for _, g := range f.Comments {
		for _, c := range g.List {
			kw, rest, ok := legacyAnnotation(c.Text)
			if !ok {
				continue
			}
			start := fset.Position(c.Pos()).Offset
			nw := "//" + directivePrefix + kw + rest
			b.Write(src[last:start])
			b.WriteString(nw)
			last = fset.Position(c.End()).Offset
			changes = append(changes, migrateChange{Line: fset.Position(c.Pos()).Line, Old: c.Text, New: nw})
		}
	}
	if len(changes) == 0 {
		return src, nil, nil
	}
	b.Write(src[last:])
	return b.Bytes(), changes, nil
}

// migrateTemplAnnotations rewrites v1 annotations in a .templ file: a line
// that is only a // comment holding a nexus annotation (the doc comment above
// a component).
func migrateTemplAnnotations(_ string, src []byte) ([]byte, []migrateChange, error) {
	lines := strings.SplitAfter(string(src), "\n")
	var changes []migrateChange
	for i, line := range lines {
		body := strings.TrimRight(line, "\r\n")
		indent := body[:len(body)-len(strings.TrimLeft(body, " \t"))]
		comment := strings.TrimRight(body[len(indent):], " \t")
		kw, rest, ok := legacyAnnotation(comment)
		if !ok {
			continue
		}
		nw := "//" + directivePrefix + kw + rest
		lines[i] = indent + nw + line[len(body):]
		changes = append(changes, migrateChange{Line: i + 1, Old: comment, New: nw})
	}
	if len(changes) == 0 {
		return src, nil, nil
	}
	return []byte(strings.Join(lines, "")), changes, nil
}
