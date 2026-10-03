package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/paulmanoni/nexus"
)

// `nexus lint --v2` reports, by file and line, what nexus 2.0 removes or
// renames in a v1 project — the same list `nexus dev` prints at boot (which
// sees only what the running app reaches), found statically. It is the
// preview of `nexus migrate v2`: every finding names the 2.0 replacement.

// v2Finding is one thing 2.0 changes in the project.
type v2Finding struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// v2Symbol is a v1 exported name 2.0 moves, renames or removes. Packages are
// relative to the nexus module ("" is the root package).
type v2Symbol struct {
	pkg, name string
	v2        string // the 2.0 replacement, as the finding states it
}

const nexusModulePath = "github.com/paulmanoni/nexus"

var v2Symbols = func() map[[2]string]string {
	rows := []v2Symbol{
		// The config package (nexus/config).
		{"", "Config", "config.Runtime"},
		{"", "ServerConfig", "config.Server"},
		{"", "WebSocketConfig", "config.WebSocket"},
		{"", "DashboardConfig", "config.Dashboard"},
		{"", "DevReloadConfig", "config.DevReload"},
		{"", "GraphQLConfig", "config.GraphQL"},
		{"", "MiddlewareConfig", "config.Middleware"},
		{"", "SecurityConfig", "config.Security"},
		{"", "CORSConfig", "config.CORS"},
		{"", "StoreConfig", "config.Stores"},
		{"", "Listener", "config.Listener"},
		{"", "ListenerScope", "config.ListenerScope"},
		{"", "ScopePublic", "config.ScopePublic"},
		{"", "ScopeInternal", "config.ScopeInternal"},
		{"", "ScopeAdmin", "config.ScopeAdmin"},
		{"", "DefaultConfigPath", "config.DefaultPath"},
		{"", "LoadConfig", "config.Load — or nexus.Boot, which loads nexus.toml itself"},
		{"", "MustLoadConfig", "config.MustLoad — or nexus.Boot, which loads nexus.toml itself"},
		{"", "Get", "config.Get"},
		{"", "MustGet", "config.MustGet"},
		{"", "HasConfig", "config.Has"},
		{"", "OnConfigChange", "config.OnChange"},
		{"", "BindConfig", "config.Bind"},
		{"", "ConfigVersion", "config.Version"},
		{"", "InstallConfigStore", "config.InstallStore"},
		{"", "UpdateConfigStore", "config.UpdateStore"},
		{"", "ClearConfigStoreForTest", "config.ResetForTest"},
		{"", "ConfigError", "config.Error"},
		{"", "LintRuntimeFile", "config.LintFile"},
		{"", "EnvVars", "config.EnvVars"},
		{"", "SkippedEnvVar", "config.SkippedEnvVar"},
		{"", "EnvVarsSkippingUnset", "config.EnvVarsSkippingUnset"},
		{"", "DotenvDefaultPath", "config.DotenvDefaultPath"},
		{"", "DatabaseSpec", "config.DatabaseSpec"},
		{"", "DatabaseSpecFor", "config.DatabaseSpecFor"},
		{"", "MustLoadDotenv", "removed: nexus.Boot loads .env itself — list a required file as [runtime] dotenv = [\"!.env\"]"},
		{"", "LoadDotenvIfPresent", "removed: nexus.Boot loads .env beside nexus.toml ([runtime] dotenv)"},
		{"", "Cache", "resource.Cache"},
		// The dev and notify packages.
		{"", "PreserveDev", "dev.Preserve"},
		{"", "PreserveDevJSON", "dev.PreserveJSON"},
		{"", "DevStateDir", "dev.StateDir"},
		{"", "DevState", "dev.State"},
		{"", "IsDev", "dev.Enabled"},
		{"", "NexusDevEnv", "dev.Env"},
		{"", "NexusDevRootEnv", "dev.RootEnv"},
		{"", "Notifier", "notify.Notifier"},
		{"", "NewNotifier", "notify.New"},
		{"", "Bus", "notify.Bus"},
		// Removals and 1:1 replacements.
		{"", "UseVolume", "nexus.DeclareVolume"},
		{"", "ServeFrontend", "nexus.Frontend (same arguments; also provides *nexus.Document)"},
		{"", "AsRestHandler", "nexus.AsRest with a handler taking *httpx.Ctx"},
		{"", "AsCRUD", "removed: declare a nexus.Resource[T] with Index/Show/Create/Update/Destroy actions"},
		{"", "Errors", "nexus.Invalid().Field(name, msg).Global(msg) (the nexus.Error model)"},
		{"", "NewErrors", "nexus.Invalid().Field(name, msg).Global(msg) (the nexus.Error model)"},
		{"", "MapCRUDError", "nexus.ErrorOf / nexus.CodeOf"},
		{"", "ErrForbidden", "nexus.Forbidden"},
		{"", "AppFromGin", "removed: inertia, view and dashboard routes find the app themselves"},
		{"", "ClientIPFromCtx", "nexus.ClientIP"},
		{"", "WithClientIP", "removed: the framework records the caller's address; read it with nexus.ClientIP"},
		{"", "GenerateDriver", "removed: nothing reads the Generate driver slot"},
		{"extension/auth", "Describe", "auth.InspectExtractor"},
		{"extension/auth", "LoginEndpoint", "auth.Config.Endpoints{Login: \"/path\"}"},
		{"extension/auth", "LogoutEndpoint", "auth.Config.Endpoints{Logout: \"/path\"}"},
		{"extension/ratelimit", "ClientIPFromCtx", "nexus.ClientIP"},
		{"extension/ratelimit", "WithClientIP", "removed: the framework records the caller's address; read it with nexus.ClientIP"},
	}
	m := make(map[[2]string]string, len(rows))
	for _, r := range rows {
		m[[2]string{r.pkg, r.name}] = r.v2
	}
	return m
}()

// v2Methods are App methods 2.0 removes, matched by name on any receiver.
var v2Methods = map[string]string{
	"UseVolume":              "App.DeclareVolume",
	"RegisterGenerateDriver": "removed: nothing reads the Generate driver slot",
	"GenerateDrivers":        "removed: nothing reads the Generate driver slot",
}

// lintV2 walks the project under root and returns every finding, ordered
// by file and line.
func lintV2(root string) ([]v2Finding, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	ignore := loadNexusIgnore(absRoot)
	var out []v2Finding
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != absRoot && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "node_modules" || name == "vendor" || name == "testdata" || ignore.match(path, true)) {
				return filepath.SkipDir
			}
			if path != absRoot {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir // a nested module is its own project
				}
			}
			return nil
		}
		if ignore.match(path, false) {
			return nil
		}
		switch {
		case strings.HasSuffix(name, "_templ.go"), name == handlerGenFileName,
			name == "nexus_imports_gen.go", name == "view_gen.go", name == "view_imports_gen.go":
			return nil
		case strings.HasSuffix(name, ".go"):
			fs, err := lintV2GoFile(path)
			if err != nil {
				return nil // not ours to report; the build will
			}
			out = append(out, fs...)
		case strings.HasSuffix(name, ".templ"):
			out = append(out, lintV2TemplFile(path)...)
		case name == "nexus.toml" && filepath.Dir(path) == absRoot:
			out = append(out, lintV2TOML(path)...)
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	// One finding per line and message (cfg.Middleware.Global = append(
	// cfg.Middleware.Global, …) names it twice).
	dedup := out[:0]
	for i, f := range out {
		if i > 0 && f == out[i-1] {
			continue
		}
		dedup = append(dedup, f)
	}
	return dedup, err
}

var legacyAnnotation = regexp.MustCompile(`^//\s*@([A-Za-z_][\w.]*)`)

// lintV2GoFile reports one Go source file.
func lintV2GoFile(path string) ([]v2Finding, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	if ast.IsGenerated(f) {
		return nil, nil
	}
	file := displayRel(path)
	var out []v2Finding
	add := func(pos token.Pos, format string, args ...any) {
		out = append(out, v2Finding{File: file, Line: fset.Position(pos).Line, Message: fmt.Sprintf(format, args...)})
	}

	// Annotations: every //@ line (gofmt's // @ included) naming a nexus
	// keyword or an exported pkg.Func decorator.
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			m := legacyAnnotation.FindStringSubmatch(c.Text)
			if m == nil || !legacyNexusKeyword(m[1]) {
				continue
			}
			add(c.Pos(), "//@%s → //nexus:%s (2.0 reads only Go's directive form; v1.80 reads both)", m[1], m[1])
		}
	}

	// Imports: the local names of nexus packages, and zap.
	local := map[string]string{} // local name → module-relative package
	usesNexus := false
	var zapImport *ast.ImportSpec
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if p == "go.uber.org/zap" {
			zapImport = spec
			continue
		}
		rel, ok := strings.CutPrefix(p, nexusModulePath)
		if !ok || (rel != "" && !strings.HasPrefix(rel, "/")) {
			continue
		}
		usesNexus = true
		rel = strings.TrimPrefix(rel, "/")
		name := filepath.Base(p)
		if rel == "" {
			name = "nexus"
		}
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "_" && name != "." {
			local[name] = rel
		}
	}
	if zapImport != nil && usesNexus {
		add(zapImport.Pos(), "go.uber.org/zap → log/slog: 2.0 logs with *slog.Logger (App.Logger, the db/cache binders and nexus.Managed take it); drop the *zap.Logger provider")
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok {
				if rel, ok := local[x.Name]; ok {
					if v2, ok := v2Symbols[[2]string{rel, n.Sel.Name}]; ok {
						add(n.Pos(), "%s.%s → %s", x.Name, n.Sel.Name, v2)
					}
					return true
				}
			}
			if n.Sel.Name == "Global" {
				if inner, ok := n.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Middleware" {
					add(n.Pos(), "Config.Middleware.Global → nexus.Middleware(…), an option whose middleware may take DI parameters and declares its stage")
				}
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok {
				if rel, isPkg := local[x.Name]; isPkg {
					if rel == "" && sel.Sel.Name == "Error" {
						add(n.Pos(), "%s.Error(err) → nexus.FailBoot(err) (nexus.Error becomes the error type)", x.Name)
					}
					if rel == "" && (sel.Sel.Name == "AsQuery" || sel.Sel.Name == "AsMutation") {
						lintV2OpName(n, x.Name, add)
					}
					if rel == "" && sel.Sel.Name == "Managed" {
						add(n.Pos(), "%s.Managed: the build func takes *slog.Logger in 2.0", x.Name)
					}
					return true
				}
			}
			if v2, ok := v2Methods[sel.Sel.Name]; ok {
				add(n.Pos(), ".%s → %s", sel.Sel.Name, v2)
			}
		case *ast.CompositeLit:
			if !isMiddlewareLit(n.Type, local) {
				return true
			}
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Gin" {
						add(kv.Pos(), "middleware.Middleware{Gin: …} → HTTP: … (the field is renamed; gin is no longer the router)")
					}
				}
			}
		case *ast.StructType:
			for _, fld := range n.Fields.List {
				if fld.Tag == nil {
					continue
				}
				tag, err := strconv.Unquote(fld.Tag.Value)
				if err != nil {
					continue
				}
				st := reflect.StructTag(tag)
				name, ok := st.Lookup("uri")
				if !ok {
					continue
				}
				if _, hasPath := st.Lookup("path"); hasPath {
					continue
				}
				name = strings.Split(name, ",")[0]
				add(fld.Pos(), "uri:%q → path:%q (2.0 binds path params from the path tag only)", name, name)
			}
		}
		return true
	})

	// Annotated NewXxx query/mutation handlers keep their v1 op name only
	// with an explicit nexus.Op.
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Doc == nil || fn.Recv != nil {
			continue
		}
		v1, v2, ok := v1OpNames(fn.Name.Name)
		if !ok {
			continue
		}
		gql, named := false, false
		for _, c := range fn.Doc.List {
			prefix, fields := splitDirective(c.Text)
			if prefix == "" || len(fields) == 0 {
				continue
			}
			switch fields[0] {
			case "query", "mutation":
				gql = true
			case "use":
				if strings.Contains(c.Text, ".Op(") {
					named = true
				}
			}
		}
		if gql && !named {
			add(fn.Pos(), "op %q from %s: 2.0 names it %q (the handler as written) — add //nexus:use nexus.Op(%q) to keep the wire name", v1, fn.Name.Name, v2, v1)
		}
	}
	return out, nil
}

// lintV2OpName reports nexus.AsQuery/AsMutation(NewXxx, …) without nexus.Op.
func lintV2OpName(call *ast.CallExpr, nexusName string, add func(token.Pos, string, ...any)) {
	if len(call.Args) == 0 {
		return
	}
	var name string
	switch h := call.Args[0].(type) {
	case *ast.Ident:
		name = h.Name
	case *ast.SelectorExpr:
		name = h.Sel.Name
	default:
		return
	}
	v1, v2, ok := v1OpNames(name)
	if !ok {
		return
	}
	for _, a := range call.Args[1:] {
		if c, ok := a.(*ast.CallExpr); ok {
			if s, ok := c.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Op" {
				return
			}
		}
	}
	add(call.Pos(), "op %q from %s: 2.0 names it %q (the handler as written) — add %s.Op(%q) to keep the wire name", v1, name, v2, nexusName, v1)
}

// v1OpNames returns the v1 and 2.0 op names of a NewXxx handler.
func v1OpNames(fn string) (v1, v2 string, ok bool) {
	if !strings.HasPrefix(fn, "New") || len(fn) <= 3 || !unicode.IsUpper(rune(fn[3])) {
		return "", "", false
	}
	rest := fn[3:]
	r := []rune(rest)
	r[0] = unicode.ToLower(r[0])
	return string(r), "new" + rest, true
}

// isMiddlewareLit reports whether a composite literal's type is
// nexus/middleware.Middleware.
func isMiddlewareLit(t ast.Expr, local map[string]string) bool {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Middleware" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && local[x.Name] == "middleware"
}

// lintV2TemplFile reports //@ directives above templ components.
func lintV2TemplFile(path string) []v2Finding {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []v2Finding
	sc := bufio.NewScanner(bytes.NewReader(src))
	for line := 1; sc.Scan(); line++ {
		m := legacyAnnotation.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		switch m[1] {
		case "page", "auth", "use":
			out = append(out, v2Finding{File: displayRel(path), Line: line,
				Message: fmt.Sprintf("//@%s → //nexus:%s (2.0 reads only Go's directive form; v1.80 reads both)", m[1], m[1])})
		}
	}
	return out
}

// v2Sections are the top-level nexus.toml tables 2.0 declares itself (the
// runtime, the binders, extensions and the deploy manifest). Any other
// table is the app's own and must be declared with config.Section[T].
var v2Sections = map[string]bool{
	"runtime": true, "databases": true, "cache": true, "storage": true, "mail": true,
	"jobs": true, "auth": true, "extensions": true, "env": true, "decorators": true,
	"profiles": true, "environments": true, "environment_overrides": true, "secrets": true,
	"files": true, "hooks": true, "tls": true, "cors": true, "errors": true,
	"services": true, "deployments": true, "peers": true,
}

// lintV2TOML reports what 2.0's strict nexus.toml rejects, and the changed
// defaults the file relies on.
func lintV2TOML(path string) []v2Finding {
	file := displayRel(path)
	var out []v2Finding
	issues, _ := nexus.LintRuntimeFile(path)
	for _, is := range issues {
		if string(is.Code) != "RUNTIME_UNKNOWN_KEY" {
			continue
		}
		out = append(out, v2Finding{File: file, Line: tomlIssueLine(is.Message),
			Message: "2.0 fails boot on this key: " + strings.TrimSpace(trimTOMLPos(is.Message))})
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var tree map[string]any
	if toml.Unmarshal(raw, &tree) != nil {
		return out
	}
	lines := strings.Split(string(raw), "\n")
	var sections []string
	for name, v := range tree {
		if _, isTable := v.(map[string]any); isTable && !v2Sections[name] {
			sections = append(sections, name)
		}
	}
	sort.Strings(sections)
	for _, name := range sections {
		out = append(out, v2Finding{File: file, Line: tomlTableLine(lines, name),
			Message: fmt.Sprintf("[%s] is an app section: 2.0 fails boot on sections nobody declared — declare it with config.Section[T](%q)", name, name)})
	}
	runtime, _ := tree["runtime"].(map[string]any)
	server, _ := runtime["server"].(map[string]any)
	if _, set := server["max_body_bytes"]; !set {
		out = append(out, v2Finding{File: file,
			Message: "[runtime.server] max_body_bytes is unset (v1: no limit): 2.0 caps request bodies at 32 MB by default — set it (or nexus.MaxBody per route) if the app accepts larger ones"})
	}
	return out
}

var tomlPos = regexp.MustCompile(`^\S+:(\d+): `)

func tomlIssueLine(msg string) int {
	if m := tomlPos.FindStringSubmatch(msg); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

func trimTOMLPos(msg string) string { return tomlPos.ReplaceAllString(msg, "") }

func tomlTableLine(lines []string, name string) int {
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "["+name+"]" || strings.HasPrefix(t, "["+name+".") {
			return i + 1
		}
	}
	return 0
}

// runLintV2 prints the findings. Advisory: they never fail the command.
func runLintV2(stdout io.Writer, root string, jsonOut bool) error {
	findings, err := lintV2(root)
	if err != nil {
		return fmt.Errorf("nexus lint --v2: %w", err)
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if findings == nil {
			findings = []v2Finding{}
		}
		return enc.Encode(struct {
			Findings []v2Finding `json:"findings"`
		}{findings})
	}
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "nexus lint --v2: nothing to change for nexus 2.0")
		return nil
	}
	files := map[string]bool{}
	for _, f := range findings {
		files[f.File] = true
		if f.Line > 0 {
			fmt.Fprintf(stdout, "%s:%d: v2: %s\n", f.File, f.Line, f.Message)
		} else {
			fmt.Fprintf(stdout, "%s: v2: %s\n", f.File, f.Message)
		}
	}
	fmt.Fprintf(stdout, "\nnexus lint --v2: %d finding(s) in %d file(s). Each is a v1 API or setting nexus 2.0 removes, renames or changes; `nexus migrate v2` rewrites the mechanical ones.\n",
		len(findings), len(files))
	return nil
}
