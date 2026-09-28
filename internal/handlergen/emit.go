// Package handlergen turns structured handler annotations into the committed
// `*_gen.go` source that registers them in decorator form (see
// decorate/DESIGN.md, Phase 2).
//
// It is intentionally decoupled from the scanner: Emit takes a plain
// []Annotation, so it is fully testable before deco.Scan exists. Phase 4 maps
// deco.Hit → handlergen.Annotation.
//
// The emitted file calls github.com/paulmanoni/nexus/decorate, so a plain
// `go build`/`go install` (which never run a generate step or overlay) compiles
// the registrations as ordinary Go — the invariant that keeps nexus
// go-install-able.
package handlergen

import (
	"bytes"
	"fmt"
	"go/format"
	"go/parser"
	"sort"
	"strconv"
	"strings"
)

// Annotation is one //@ directive found on a function. A function may carry one
// PRIMARY annotation (the registration kind) plus zero or more MODIFIER
// annotations (auth, …) that become per-op options.
type Annotation struct {
	Func    string   // the annotated function's name (same package as the generated file)
	Keyword string   // "rest","query","mutation","subscription","ws","worker","provide","auth","use"
	Args    []string // raw tokens after the keyword
	File    string   // source file of the directive (as the caller wants it shown in errors)
	Line    int      // source line — errors point here; also gives statements a stable order
	Imports []string // import lines this directive's expression needs (//@use); e.g. `"github.com/x/rl"`
}

// errf builds an error anchored at the annotation's source position, in the
// standard file:line: form editors and CI understand. Callers that construct
// Annotations without File (library use) still get the function context.
func (a Annotation) errf(format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if a.File != "" {
		return fmt.Errorf("%s:%d: %s", a.File, a.Line, msg)
	}
	return fmt.Errorf("%s: %s", a.Func, msg)
}

// Config controls the generated file.
type Config struct {
	Package        string // package clause of the generated file (required)
	Module         string // nexus.Module name for the group; defaults to Package
	NexusImport    string // default github.com/paulmanoni/nexus
	DecorateImport string // default github.com/paulmanoni/nexus/decorate
	AuthImport     string // default github.com/paulmanoni/nexus/extension/auth
}

// DefaultModule is the dashboard module name used when annotations live in the
// main package (or a package whose name makes a poor module label) — so those
// endpoints still group under a sensible node instead of "main".
const DefaultModule = "app"

func (c *Config) applyDefaults() {
	if c.Module == "" {
		c.Module = c.Package
	}
	// Derive the module from the handlers' package name; fall back to a friendly
	// default for the main package so it reads well on the dashboard.
	if c.Module == "" || c.Module == "main" {
		c.Module = DefaultModule
	}
	if c.NexusImport == "" {
		c.NexusImport = "github.com/paulmanoni/nexus"
	}
	if c.DecorateImport == "" {
		c.DecorateImport = "github.com/paulmanoni/nexus/decorate"
	}
	if c.AuthImport == "" {
		c.AuthImport = "github.com/paulmanoni/nexus/extension/auth"
	}
}

var primaryKeywords = map[string]bool{
	"provide": true, "rest": true, "query": true, "mutation": true,
	"subscription": true, "ws": true, "worker": true,
}

var modifierKeywords = map[string]bool{"auth": true, "use": true}

// isPrimaryKeyword reports whether kw registers an endpoint/provider. A keyword
// containing a dot (e.g. "inertia.Page") is a CUSTOM extension decorator: it
// emits a verbatim pkg.Func(args…, fn) call (the package import is supplied via
// Annotation.Imports by the caller). Built-in primaries are the fixed set.
func isPrimaryKeyword(kw string) bool {
	return primaryKeywords[kw] || strings.Contains(kw, ".")
}

// optsAllowed reports whether a primary kind accepts per-op modifiers.
func optsAllowed(kind string) bool {
	switch kind {
	case "rest", "query", "mutation", "subscription", "ws":
		return true
	}
	// Custom extension decorators (//@pkg.Func, e.g. //@inertia.Page) accept
	// modifiers too: they're appended as trailing options to the registrar call
	// (inertia.Page(..., auth.Required())). The registrar must accept the option
	// type — a compile error if it doesn't, which is the right, loud failure.
	return strings.Contains(kind, ".")
}

// Emit renders the generated source for one package. Returns (nil, nil) when
// anns contains no primary registration (caller should then write no file).
func Emit(cfg Config, anns []Annotation) ([]byte, error) {
	cfg.applyDefaults()
	if cfg.Package == "" {
		return nil, fmt.Errorf("handlergen: Config.Package is required")
	}

	// imports accumulates the ready-to-emit import lines the file needs.
	// decorate and nexus are always present (the file calls decorate.Register
	// with a nexus.Module); auth/use/custom-decorators add theirs on demand.
	// gofmt (via format.Source) sorts them within the block.
	imports := map[string]bool{
		strconv.Quote(cfg.DecorateImport): true,
		strconv.Quote(cfg.NexusImport):    true,
	}

	// Group annotations per function, preserving first-seen line for ordering.
	type group struct {
		fn        string
		primary   *Annotation
		modifiers []Annotation
		line      int
	}
	order := []string{}
	groups := map[string]*group{}
	for i := range anns {
		a := anns[i]
		g, ok := groups[a.Func]
		if !ok {
			g = &group{fn: a.Func, line: a.Line}
			groups[a.Func] = g
			order = append(order, a.Func)
		}
		switch {
		case isPrimaryKeyword(a.Keyword):
			if g.primary != nil {
				return nil, a.errf("%s has two primary annotations (//@%s at line %d and //@%s) — a function registers exactly once",
					a.Func, g.primary.Keyword, g.primary.Line, a.Keyword)
			}
			p := a
			g.primary = &p
			g.line = a.Line
			// A custom decorator carries its package import.
			for _, imp := range a.Imports {
				imports[imp] = true
			}
		case modifierKeywords[a.Keyword]:
			g.modifiers = append(g.modifiers, a)
		default:
			return nil, a.errf("%s has unknown annotation //@%s", a.Func, a.Keyword)
		}
	}

	type stmt struct {
		line int
		text string
	}
	var stmts []stmt
	for _, fn := range order {
		g := groups[fn]
		if g.primary == nil {
			return nil, g.modifiers[0].errf("%s has modifier annotations but no primary — add //@rest, //@query, //@mutation, //@subscription, //@ws, //@worker or //@provide", fn)
		}
		if len(g.modifiers) > 0 && !optsAllowed(g.primary.Keyword) {
			m := g.modifiers[0]
			return nil, m.errf("//@%s on %s does not accept modifier annotations (//@%s applies to rest/query/mutation/subscription/ws and custom decorators)",
				g.primary.Keyword, fn, m.Keyword)
		}
		opts, optImports, err := renderOpts(g.modifiers, cfg.AuthImport)
		if err != nil {
			return nil, err
		}
		for _, imp := range optImports {
			imports[imp] = true
		}
		text, err := renderPrimary(*g.primary, fn, opts)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, stmt{line: g.line, text: text})
	}
	if len(stmts) == 0 {
		return nil, nil
	}
	sort.SliceStable(stmts, func(i, j int) bool { return stmts[i].line < stmts[j].line })

	importLines := make([]string, 0, len(imports))
	for imp := range imports {
		importLines = append(importLines, imp)
	}
	sort.Strings(importLines)

	var b bytes.Buffer
	b.WriteString("// Code generated by \"nexus generate handlers\"; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "package %s\n\n", cfg.Package)
	b.WriteString("import (\n")
	for _, imp := range importLines {
		fmt.Fprintf(&b, "\t%s\n", imp)
	}
	b.WriteString(")\n\n")
	// One Register call per package, wrapping the registrations in an explicit
	// nexus.Module(<package>, …) so the grouping is visible in the generated
	// code. nexus.Boot/Run auto-drains the registry — no decorate.Module(...)
	// call in the app.
	b.WriteString("func init() {\n")
	fmt.Fprintf(&b, "\tdecorate.Register(nexus.Module(%s,\n", strconv.Quote(cfg.Module))
	for _, s := range stmts {
		fmt.Fprintf(&b, "\t\t%s,\n", s.text)
	}
	b.WriteString("\t))\n}\n")

	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("handlergen: format generated source: %w\n---\n%s", err, b.String())
	}
	return out, nil
}

// renderPrimary builds the nexus.Option EXPRESSION for a primary annotation
// (the Emit caller wraps all of a package's expressions in one
// decorate.Register("module", …) call). Built-in keywords map to the real
// nexus.As*/Provide builders; a dotted keyword is a custom extension decorator
// whose registrar already returns a nexus.Option, so it's emitted as-is.
func renderPrimary(a Annotation, fn string, opts []string) (string, error) {
	optTail := ""
	if len(opts) > 0 {
		optTail = ", " + strings.Join(opts, ", ")
	}
	switch a.Keyword {
	case "provide":
		if len(a.Args) != 0 {
			return "", a.errf("//@provide takes no arguments (got %v)", a.Args)
		}
		return fmt.Sprintf("nexus.Provide(%s)", fn), nil
	case "worker":
		if len(a.Args) != 1 || a.Args[0] == "" {
			return "", a.errf("//@worker needs exactly a <name> (got %v)", a.Args)
		}
		return fmt.Sprintf("nexus.AsWorker(%s, %s)", strconv.Quote(a.Args[0]), fn), nil
	case "rest":
		if len(a.Args) != 2 {
			return "", a.errf("//@rest needs <METHOD> <PATH>, e.g. //@rest GET /users/:id (got %v)", a.Args)
		}
		method, err := restMethod(a)
		if err != nil {
			return "", err
		}
		if err := checkRoutePath(a, "rest", a.Args[1]); err != nil {
			return "", err
		}
		return fmt.Sprintf("nexus.AsRest(%s, %s, %s%s)",
			strconv.Quote(method), strconv.Quote(a.Args[1]), fn, optTail), nil
	case "ws":
		if len(a.Args) != 2 {
			return "", a.errf("//@ws needs <PATH> <TYPE>, e.g. //@ws /events chat.send (got %v)", a.Args)
		}
		if err := checkRoutePath(a, "ws", a.Args[0]); err != nil {
			return "", err
		}
		return fmt.Sprintf("nexus.AsWS(%s, %s, %s%s)",
			strconv.Quote(a.Args[0]), strconv.Quote(a.Args[1]), fn, optTail), nil
	case "query", "mutation", "subscription":
		if len(a.Args) != 0 {
			return "", a.errf("//@%s takes no arguments (got %v) — the op name derives from the function name; "+
				"to override it, add `//@use nexus.Op(%q)`", a.Keyword, a.Args, a.Args[0])
		}
		builder := map[string]string{"query": "AsQuery", "mutation": "AsMutation", "subscription": "AsSubscription"}[a.Keyword]
		return fmt.Sprintf("nexus.%s(%s%s)", builder, fn, optTail), nil
	}
	// Custom extension decorator: //@pkg.Func args… → pkg.Func(args…, fn). The
	// registrar returns a nexus.Option (the universal nexus convention —
	// inertia.Page, etc.), so any existing one works with no wrapper. Each
	// whitespace-separated token is a distinct argument (comma-joined). Modifiers
	// (//@auth, //@use) are appended as trailing options (optTail), so e.g.
	// //@inertia.Page + //@auth Required → inertia.Page(args…, fn, auth.Required()).
	// The registrar must accept the option type (a compile error otherwise).
	if strings.Contains(a.Keyword, ".") {
		if len(a.Args) > 0 {
			return fmt.Sprintf("%s(%s, %s%s)", a.Keyword, strings.Join(a.Args, ", "), fn, optTail), nil
		}
		return fmt.Sprintf("%s(%s%s)", a.Keyword, fn, optTail), nil
	}
	return "", a.errf("unhandled primary //@%s", a.Keyword)
}

// restMethod validates //@rest's METHOD token against the HTTP verbs and
// normalises casing, so `//@rest get /users` registers as GET instead of an
// unroutable literal "get".
func restMethod(a Annotation) (string, error) {
	method := strings.ToUpper(a.Args[0])
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return method, nil
	}
	return "", a.errf("//@rest method %q is not an HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)", a.Args[0])
}

// checkRoutePath insists a route path starts with "/", the mistake that
// otherwise surfaces only as a route that never matches.
func checkRoutePath(a Annotation, kw, path string) error {
	if !strings.HasPrefix(path, "/") {
		return a.errf("//@%s path %q must start with \"/\"", kw, path)
	}
	return nil
}

// renderOpts turns modifier annotations into Go option expressions and the
// import lines they require. authImport is the import for //@auth directives.
func renderOpts(mods []Annotation, authImport string) (exprs []string, imports []string, err error) {
	for _, m := range mods {
		switch m.Keyword {
		case "auth":
			if len(m.Args) == 0 {
				return nil, nil, m.errf("//@auth needs Required or Requires(\"ROLE\")")
			}
			expr := strings.Join(m.Args, " ")
			if !strings.Contains(expr, "(") {
				expr += "()" // //@auth Required → auth.Required()
			}
			expr = "auth." + expr
			if err := checkExpr(m, expr); err != nil {
				return nil, nil, err
			}
			exprs = append(exprs, expr)
			imports = append(imports, strconv.Quote(authImport))
		case "use":
			// //@use <expr> emits the expression verbatim as a per-op option.
			// Its package imports are resolved by the caller (the CLI reads the
			// annotated file's import block) and supplied in Imports.
			expr := strings.Join(m.Args, " ")
			if expr == "" {
				return nil, nil, m.errf("//@use needs a middleware expression, e.g. //@use ratelimit.Per(time.Minute, 60)")
			}
			if err := checkExpr(m, expr); err != nil {
				return nil, nil, err
			}
			exprs = append(exprs, expr)
			imports = append(imports, m.Imports...)
		default:
			return nil, nil, m.errf("unhandled modifier //@%s", m.Keyword)
		}
	}
	return exprs, imports, nil
}

// checkExpr rejects an option expression that isn't parseable Go AT THE
// ANNOTATION, instead of letting it explode later as a syntax error inside
// the generated file where the source of the text is invisible.
func checkExpr(m Annotation, expr string) error {
	if _, err := parser.ParseExpr(expr); err != nil {
		return m.errf("//@%s expression %q is not valid Go: %v", m.Keyword, expr, err)
	}
	return nil
}
