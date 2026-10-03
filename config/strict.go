package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus/v2/internal/extnames"
	"github.com/paulmanoni/nexus/v2/manifest"
)

// nexus.toml is strict: every table in it is declared by an owner, and every
// key in a declared table is one its type has a field for. The owners are
//
//   - this package: [runtime], [databases], [env], [extensions], the
//     [decorators] codegen hint, and the deploy-manifest tables;
//   - framework extensions, through Section in their package ([cache],
//     [storage], [mail], [jobs]) — linked only when the app imports them;
//   - extension decoders (nexus.RegisterExtensionDecoder) for
//     [extensions.<name>], with DeclareExtension for their keys;
//   - the app, through Section.
//
// A table nobody declared, a key at the top level, or a key a declared table
// has no field for fails the load with the file position and a did-you-mean.

// decoratorsBlock is [decorators], read by the nexus CLI's handler codegen.
type decoratorsBlock struct {
	// Imports maps a decorator's package selector to its import path.
	Imports map[string]string `toml:"imports"`
}

// builtinTables are the tables this package declares, in document order.
var builtinTables = []struct {
	name string
	typ  reflect.Type
}{
	{"runtime", reflect.TypeFor[runtimeBlock]()},
	{"databases", reflect.TypeFor[map[string]DatabaseSpec]()},
	{"env", reflect.TypeFor[map[string]any]()},
	{"extensions", nil}, // built per load from the registered decoders
	{"decorators", reflect.TypeFor[decoratorsBlock]()},
	// The deploy-manifest surface (manifest.DeployTOMLInputs).
	{"environments", reflect.TypeFor[map[string]manifest.EnvironmentTOML]()},
	{"environment_overrides", reflect.TypeFor[map[string]manifest.Override]()},
	{"secrets", reflect.TypeFor[map[string]manifest.Secret]()},
	{"files", reflect.TypeFor[map[string]manifest.File]()},
	{"hooks", reflect.TypeFor[*manifest.Hooks]()},
	{"tls", reflect.TypeFor[*manifest.TLSBlock]()},
	{"cors", reflect.TypeFor[*manifest.CORSBlock]()},
	{"errors", reflect.TypeFor[*manifest.ErrorsBlock]()},
	{"services", reflect.TypeFor[map[string]manifest.ServiceNeed]()},
	{"deployments", reflect.TypeFor[map[string]any]()},
	{"peers", reflect.TypeFor[map[string]any]()},
}

func init() {
	for _, b := range builtinTables {
		declare(&sectionDecl{name: b.name, typ: b.typ})
	}
}

// Problem is one key or table of nexus.toml that nothing declares.
type Problem struct {
	// Path is the TOML table path, e.g. ["runtime","server","adress"].
	Path []string
	// Line and Column locate it in the file (1-based).
	Line, Column int
	// Kind classifies it: "key" (a key its table has no field for),
	// "misplaced" (a key at the top level), "section" (an undeclared
	// top-level table) or "extension" (an [extensions.x] block without a
	// registered decoder).
	Kind string
	// Message says what is wrong; Hint, when non-empty, what was meant.
	Message, Hint string
}

// Key is the dotted form of Path.
func (p Problem) Key() string { return strings.Join(p.Path, ".") }

// String renders "line N: message — hint".
func (p Problem) String() string {
	s := fmt.Sprintf("line %d: %s", p.Line, p.Message)
	if p.Hint != "" {
		s += " — " + p.Hint
	}
	return s
}

// document is one strict decode of nexus.toml.
type document struct {
	source   string
	value    reflect.Value // *docType
	decls    []*sectionDecl
	tree     map[string]any
	problems []Problem
}

// documentType builds the struct the whole file decodes into: one field per
// declared table, in declaration order. [extensions] is a struct of the
// registered extension names, so an unregistered one is an unknown key.
func documentType() (reflect.Type, []*sectionDecl) {
	decls := declaredSections()
	fields := make([]reflect.StructField, 0, len(decls))
	for i, d := range decls {
		t := d.typ
		if d.name == "extensions" {
			t = extensionsType()
		}
		fields = append(fields, reflect.StructField{
			Name: fmt.Sprintf("F%d", i),
			Type: t,
			Tag:  reflect.StructTag(fmt.Sprintf(`toml:%q`, d.name)),
		})
	}
	return reflect.StructOf(fields), decls
}

func extensionsType() reflect.Type {
	names := extnames.List()
	sectionsMu.RLock()
	defer sectionsMu.RUnlock()
	fields := make([]reflect.StructField, 0, len(names))
	for i, n := range names {
		t := extensionSchemas[n]
		if t == nil {
			t = reflect.TypeFor[map[string]any]()
		}
		fields = append(fields, reflect.StructField{
			Name: fmt.Sprintf("E%d", i),
			Type: t,
			Tag:  reflect.StructTag(fmt.Sprintf(`toml:%q`, n)),
		})
	}
	return reflect.StructOf(fields)
}

// decodeDocument decodes expanded strictly. A syntax or type error is
// returned as an *Error; keys nothing declares are collected as problems
// (the rest of the document still decodes).
func decodeDocument(expanded []byte, source string) (*document, error) {
	typ, decls := documentType()
	v := reflect.New(typ)
	for i, d := range decls {
		if d.def != nil {
			v.Elem().Field(i).Set(d.def())
		}
	}
	dec := toml.NewDecoder(bytes.NewReader(expanded))
	dec.DisallowUnknownFields()
	err := dec.Decode(v.Interface())
	var strict *toml.StrictMissingError
	if err != nil && !errors.As(err, &strict) {
		return nil, newConfigError("parse", source, err)
	}
	doc := &document{source: source, value: v, decls: decls}
	if err := toml.Unmarshal(expanded, &doc.tree); err != nil {
		return nil, newConfigError("parse", source, err)
	}
	if strict != nil {
		doc.problems = classifyProblems(strict, doc.tree, typ)
	}
	return doc, nil
}

func (d *document) field(name string) reflect.Value {
	for i, decl := range d.decls {
		if decl.name == name {
			return d.value.Elem().Field(i)
		}
	}
	return reflect.Value{}
}

func (d *document) runtime() runtimeBlock {
	b, _ := d.field("runtime").Interface().(runtimeBlock)
	return b
}

func (d *document) databases() map[string]DatabaseSpec {
	m, _ := d.field("databases").Interface().(map[string]DatabaseSpec)
	return m
}

// publishSections hands each Section handle its decoded table.
func (d *document) publishSections() error {
	for i, decl := range d.decls {
		if decl.set == nil {
			continue
		}
		_, present := d.tree[decl.name]
		if err := decl.set(d.value.Elem().Field(i), present); err != nil {
			return err
		}
	}
	return nil
}

// classifyProblems turns go-toml's unknown-key list into problems, one per
// undeclared table (not one per key inside it), in source order.
func classifyProblems(strict *toml.StrictMissingError, tree map[string]any, typ reflect.Type) []Problem {
	schema := buildConfigSchema(typ)
	leaves := schemaLeaves(schema)
	seen := map[string]bool{}
	var out []Problem
	for i := range strict.Errors {
		de := &strict.Errors[i]
		path := append([]string(nil), de.Key()...)
		if len(path) == 0 {
			continue
		}
		line, col := de.Position()
		p := Problem{Path: path, Line: line, Column: col}
		table := isTOMLTable(tree, path)
		switch {
		case len(path) == 1 && !table:
			p.Kind = "misplaced"
			p.Message = fmt.Sprintf("%q is set at the top level of the file, outside any table, where nothing reads it", path[0])
			p.Hint = unknownConfigKeyHint(schema, leaves, path)
		case !IsDeclared(path[0]):
			if seen[path[0]] {
				continue
			}
			seen[path[0]] = true
			p.Path = path[:1]
			p.Kind = "section"
			p.Message = fmt.Sprintf("[%s] is not a declared section", path[0])
			p.Hint = undeclaredSectionHint(path[0])
		case path[0] == "extensions" && len(path) >= 2 && !extnames.Has(path[1]):
			key := "extensions." + path[1]
			if seen[key] {
				continue
			}
			seen[key] = true
			p.Path = path[:2]
			p.Kind = "extension"
			p.Message = fmt.Sprintf("[extensions.%s] has no registered decoder", path[1])
			p.Hint = "import the extension package that provides it (a blank import is enough)"
			if names := extnames.List(); len(names) > 0 {
				p.Hint += "; registered: " + strings.Join(names, ", ")
			}
		case table:
			p.Kind = "key"
			parent := strings.Join(path[:len(path)-1], ".")
			p.Message = fmt.Sprintf("[%s] is not a table of [%s]", strings.Join(path, "."), parent)
			p.Hint = unknownConfigKeyHint(schema, leaves, path)
		default:
			p.Kind = "key"
			p.Message = fmt.Sprintf("%q is not a key of [%s]", path[len(path)-1], strings.Join(path[:len(path)-1], "."))
			p.Hint = unknownConfigKeyHint(schema, leaves, path)
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// undeclaredSectionHint says how to make [name] legal: import the framework
// package that owns it, fix a misspelt name, or declare it.
func undeclaredSectionHint(name string) string {
	if owner := SectionOwner(name); owner != "" {
		return fmt.Sprintf("[%s] is declared by %s — import it (the app reads [%s] through it)", name, owner, name)
	}
	if near := nearestName(name, Sections()); near != "" {
		return fmt.Sprintf("did you mean [%s]?", near)
	}
	return fmt.Sprintf(`declare it in Go: var %s = config.Section[T](%q) — or remove it`, exportedIdent(name), name)
}

// exportedIdent turns a table name into a plausible Go identifier for hints.
func exportedIdent(name string) string {
	var b strings.Builder
	up := true
	for _, r := range name {
		switch {
		case r == '_' || r == '-':
			up = true
		case up:
			b.WriteString(strings.ToUpper(string(r)))
			up = false
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "Section"
	}
	return b.String()
}

// newKeysError renders problems as one *Error. A single problem reads like
// any config error; several list one per line beneath the first.
func newKeysError(source string, problems []Problem) *Error {
	first := problems[0]
	e := &Error{Source: source, Stage: "check keys", Line: first.Line, Problems: problems}
	if len(problems) == 1 {
		e.Err = errors.New(first.Message)
		e.Hint = first.Hint
		return e
	}
	e.Err = fmt.Errorf("%d keys or tables nothing declares", len(problems))
	var b strings.Builder
	for _, p := range problems {
		fmt.Fprintf(&b, "%s:%d: %s", source, p.Line, p.Message)
		if p.Hint != "" {
			b.WriteString(" — " + p.Hint)
		}
		b.WriteByte('\n')
	}
	e.Snippet = b.String()
	e.Hint = "nexus.toml is strict: fix the spelling or nesting, or declare the table (config.Section) — `nexus config check` lists the same problems"
	return e
}

// Check validates nexus.toml text against the declared tables without
// loading it: no config.Get seeding, no [env] export, no section decode. It
// returns the problems found, or an *Error for a file that does not parse.
// Callers outside a running app (the nexus CLI) declare the app's own
// sections first.
func Check(raw []byte, source string) ([]Problem, error) {
	// ${VAR} placeholders only ever sit inside strings, so the unexpanded
	// text has the same keys and types — and CI need not set the secrets.
	doc, err := decodeDocument(raw, source)
	if err != nil {
		return nil, err
	}
	return doc.problems, nil
}
