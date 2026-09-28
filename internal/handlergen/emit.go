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
	Path           string // nexus.Path prefix for the module (REST + GraphQL); "" = none
	RoutePrefix    string // nexus.RoutePrefix (REST-only) for the module; "" = none
	NexusImport    string // default github.com/paulmanoni/nexus
	DecorateImport string // default github.com/paulmanoni/nexus/decorate
	AuthImport     string // default github.com/paulmanoni/nexus/extension/auth

	// RouterDecls are this package's //@router declarations, emitted as
	// nexus.RouterDecl calls; KnownRouters is every declared name across the
	// scan, for validating //@on references.
	RouterDecls  []RouterDecl
	KnownRouters map[string]bool
}

// RouterDecl is one //@router declaration ready to emit.
type RouterDecl struct {
	Name, Prefix, Parent string
	AuthExpr             string // "", or e.g. `auth.Requires("ADMIN")`
}

// packageDirectiveKeywords are the //@ directives that live on the PACKAGE
// doc comment and configure the whole generated module, rather than
// registering a function.
var packageDirectiveKeywords = map[string]bool{"module": true, "path": true, "routeprefix": true}

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

var modifierKeywords = map[string]bool{"auth": true, "on": true, "session": true, "use": true}

// sessionImportPath is the sessions extension, for the //@session modifier.
const sessionImportPath = "github.com/paulmanoni/nexus/extension/session"

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
			if err := normalizeKnownDecorator(&a); err != nil {
				return nil, err
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
		mods, onRouter, err := extractOnRouter(g.modifiers, cfg.KnownRouters)
		if err != nil {
			return nil, err
		}
		opts, optImports, err := renderOpts(mods, cfg.AuthImport)
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
		if onRouter != "" {
			// The op registers on the named router (assembled by the runtime)
			// instead of the package module.
			text = fmt.Sprintf("nexus.OnRouter(%s, %s)", strconv.Quote(onRouter), text)
		}
		stmts = append(stmts, stmt{line: g.line, text: text})
	}
	for _, rd := range cfg.RouterDecls {
		if rd.AuthExpr != "" {
			imports[strconv.Quote(cfg.AuthImport)] = true
		}
	}
	if len(stmts) == 0 && len(cfg.RouterDecls) == 0 {
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
	// Package-level prefixes lead the option list, mirroring hand-written
	// module wiring.
	if cfg.Path != "" {
		fmt.Fprintf(&b, "\t\tnexus.Path(%s),\n", strconv.Quote(cfg.Path))
	}
	if cfg.RoutePrefix != "" {
		fmt.Fprintf(&b, "\t\tnexus.RoutePrefix(%s),\n", strconv.Quote(cfg.RoutePrefix))
	}
	// //@router declarations record into the runtime's router registry; the
	// tree assembles once every package init has run.
	for _, rd := range cfg.RouterDecls {
		if rd.AuthExpr != "" {
			fmt.Fprintf(&b, "\t\tnexus.RouterDecl(%s, %s, %s, %s),\n",
				strconv.Quote(rd.Name), strconv.Quote(rd.Prefix), strconv.Quote(rd.Parent), rd.AuthExpr)
		} else {
			fmt.Fprintf(&b, "\t\tnexus.RouterDecl(%s, %s, %s),\n",
				strconv.Quote(rd.Name), strconv.Quote(rd.Prefix), strconv.Quote(rd.Parent))
		}
	}
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

// inertiaImportPath identifies the inertia extension however its import is
// aliased, so //@inertia.Page (or //@in.Page) gets first-class argument
// handling below.
const inertiaImportPath = "github.com/paulmanoni/nexus/extension/inertia"

// normalizeKnownDecorator rewrites the argument list of well-known extension
// decorators so their annotations read naturally — bare tokens instead of
// hand-quoted Go strings — and malformed values fail AT the annotation with
// file:line, not as a compile error inside the generated file. Decorators the
// table doesn't know pass through verbatim, as before.
func normalizeKnownDecorator(a *Annotation) error {
	if strings.HasSuffix(a.Keyword, ".Page") && importsHavePath(a.Imports, inertiaImportPath) {
		return normalizeInertiaPage(a)
	}
	return nil
}

// normalizeInertiaPage handles //@inertia.Page <METHOD> <PATH> <Component>.
// Tokens may be bare (GET /users Users/Index) or quoted; the method accepts
// the comma multi-verb form (get,post → "GET,POST") in any case.
func normalizeInertiaPage(a *Annotation) error {
	if len(a.Args) != 3 {
		return a.errf("//@%s needs <METHOD> <PATH> <Component>, e.g. //@%s GET /users Users/Index (got %v)",
			a.Keyword, a.Keyword, a.Args)
	}
	method, err := decoratorToken(a, a.Args[0])
	if err != nil {
		return err
	}
	verbs := strings.Split(method, ",")
	for i, v := range verbs {
		verbs[i] = strings.ToUpper(strings.TrimSpace(v))
		switch verbs[i] {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		default:
			return a.errf("//@%s method %q is not an HTTP method (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)", a.Keyword, v)
		}
	}
	path, err := decoratorToken(a, a.Args[1])
	if err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/") {
		return a.errf("//@%s path %q must start with \"/\"", a.Keyword, path)
	}
	component, err := decoratorToken(a, a.Args[2])
	if err != nil {
		return err
	}
	if component == "" {
		return a.errf("//@%s component name is empty — name the client component, e.g. Users/Index", a.Keyword)
	}
	a.Args = []string{
		strconv.Quote(strings.Join(verbs, ",")),
		strconv.Quote(path),
		strconv.Quote(component),
	}
	return nil
}

// decoratorToken returns a directive token's value: a quoted token is
// unquoted (and must be well-formed), a bare token is itself.
func decoratorToken(a *Annotation, tok string) (string, error) {
	if !strings.HasPrefix(tok, `"`) {
		return tok, nil
	}
	v, err := strconv.Unquote(tok)
	if err != nil {
		return "", a.errf("//@%s argument %s is not a valid quoted string", a.Keyword, tok)
	}
	return v, nil
}

// importsHavePath reports whether any of the annotation's import lines
// (`"path"` or `alias "path"`) names path.
func importsHavePath(lines []string, path string) bool {
	for _, l := range lines {
		if i := strings.IndexByte(l, '"'); i >= 0 {
			if p, err := strconv.Unquote(l[i:]); err == nil && p == path {
				return true
			}
		}
	}
	return false
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
			expr, needsAuth, err := renderAuthOption(m)
			if err != nil {
				return nil, nil, err
			}
			exprs = append(exprs, expr)
			if needsAuth {
				imports = append(imports, strconv.Quote(authImport))
			}
		case "session":
			expr, err := renderSessionOption(m)
			if err != nil {
				return nil, nil, err
			}
			exprs = append(exprs, expr)
			imports = append(imports, strconv.Quote(sessionImportPath))
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

// renderAuthOption turns an //@auth modifier into its option expression.
// The grammar reads naturally — bare tokens, capability case-insensitive:
//
//	//@auth Required                → auth.Required()
//	//@auth Requires ADMIN HR       → auth.Requires("ADMIN", "HR")
//	//@auth Public                  → nexus.Public() (deny-by-default opt-out)
//	//@auth Requires("ADMIN", "HR") → unchanged (legacy call form)
//
// Unknown capabilities fail at the annotation with a suggestion, so a typo
// never becomes an undefined identifier inside the generated file.
// needsAuthImport is false for Public, which lives in the nexus core.
func renderAuthOption(m Annotation) (expr string, needsAuthImport bool, err error) {
	if len(m.Args) == 0 {
		return "", false, m.errf("//@auth needs a capability: Required, Requires <PERM…>, or Public")
	}
	head := m.Args[0]

	// Legacy call form: //@auth Requires("A", "B") / Required(). Validate the
	// capability, pass the expression through parse-checked.
	if i := strings.IndexByte(head, '('); i >= 0 {
		switch cap := head[:i]; cap {
		case "Required", "Requires":
			expr := "auth." + strings.Join(m.Args, " ")
			return expr, true, checkExpr(m, expr)
		case "Public":
			return "nexus.Public()", false, nil
		default:
			return "", false, m.errf("unknown //@auth capability %q — use Required, Requires <PERM…>, or Public%s",
				cap, authSuggestion(cap))
		}
	}

	switch strings.ToLower(head) {
	case "required":
		if len(m.Args) > 1 {
			return "", false, m.errf("//@auth Required takes no arguments (got %v) — to require permissions: //@auth Requires %s",
				m.Args[1:], strings.Join(m.Args[1:], " "))
		}
		return "auth.Required()", true, nil
	case "public":
		if len(m.Args) > 1 {
			return "", false, m.errf("//@auth Public takes no arguments (got %v)", m.Args[1:])
		}
		return "nexus.Public()", false, nil
	case "requires":
		perms := m.Args[1:]
		if len(perms) == 0 {
			return "", false, m.errf("//@auth Requires needs at least one permission, e.g. //@auth Requires ADMIN")
		}
		quoted := make([]string, len(perms))
		for i, p := range perms {
			v, err := decoratorToken(&m, p)
			if err != nil {
				return "", false, err
			}
			if v == "" {
				return "", false, m.errf("//@auth Requires has an empty permission (got %v)", m.Args)
			}
			quoted[i] = strconv.Quote(v)
		}
		return "auth.Requires(" + strings.Join(quoted, ", ") + ")", true, nil
	default:
		return "", false, m.errf("unknown //@auth capability %q — use Required, Requires <PERM…>, or Public%s",
			head, authSuggestion(head))
	}
}

// extractOnRouter pulls the //@on modifier out of a function's modifier list,
// validating its shape and that the named router is declared somewhere in the
// scan (with a did-you-mean for near misses).
func extractOnRouter(mods []Annotation, known map[string]bool) (rest []Annotation, router string, err error) {
	for _, m := range mods {
		if m.Keyword != "on" {
			rest = append(rest, m)
			continue
		}
		if router != "" {
			return nil, "", m.errf("//@on given twice — an op registers on one router")
		}
		if len(m.Args) != 1 || m.Args[0] == "" {
			return nil, "", m.errf("//@on needs exactly a router name, e.g. //@on billing (got %v)", m.Args)
		}
		name := m.Args[0]
		if !known[name] {
			names := make([]string, 0, len(known))
			for n := range known {
				names = append(names, n)
			}
			sort.Strings(names)
			hint := ""
			for _, n := range names {
				if editDistance(strings.ToLower(name), strings.ToLower(n)) <= 2 {
					hint = fmt.Sprintf(" (did you mean %q?)", n)
					break
				}
			}
			declared := "none declared — add //@router <name> <prefix> to a package doc comment"
			if len(names) > 0 {
				declared = "declared: " + strings.Join(names, ", ")
			}
			return nil, "", m.errf("//@on names unknown router %q (%s)%s", name, declared, hint)
		}
		router = name
	}
	return rest, router, nil
}

// renderSessionOption turns a //@session modifier into its option
// expression. The one capability is Required (case-insensitive, bare or the
// Required() call form) — the flow-continuity gate session.Required():
//
//	//@session Required → session.Required()
func renderSessionOption(m Annotation) (string, error) {
	if len(m.Args) == 0 {
		return "", m.errf("//@session needs a capability: Required")
	}
	head := m.Args[0]
	if head == "Required()" || strings.EqualFold(head, "required") {
		if len(m.Args) > 1 {
			return "", m.errf("//@session Required takes no arguments (got %v)", m.Args[1:])
		}
		return "session.Required()", nil
	}
	hint := ""
	if editDistance(strings.ToLower(strings.TrimSuffix(head, "()")), "required") <= 2 {
		hint = " (did you mean Required?)"
	}
	return "", m.errf("unknown //@session capability %q — use Required%s", head, hint)
}

// authSuggestion returns a did-you-mean hint for a near-miss capability.
func authSuggestion(got string) string {
	lower := strings.ToLower(got)
	for _, cap := range []string{"Required", "Requires", "Public"} {
		if d := editDistance(lower, strings.ToLower(cap)); d <= 2 {
			return fmt.Sprintf(" (did you mean %s?)", cap)
		}
	}
	return ""
}

// editDistance is the Levenshtein distance between two short ASCII words.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
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
