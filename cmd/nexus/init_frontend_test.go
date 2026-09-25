package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitFrontend_OnGoOnlyProject is the headline path: an
// existing Go project with no frontend gets a Vite project under web/
// in one command, and main.go gets patched to embed and serve it.
func TestInitFrontend_OnGoOnlyProject(t *testing.T) {
	dir := t.TempDir()
	// Stage a minimal go-only main.go — same shape `nexus new`
	// (no --frontend) produces.
	mainGo := `package main

import "github.com/paulmanoni/nexus"

func main() {
	nexus.Run(
		nexus.Config{
			Server:    nexus.ServerConfig{Addr: ":8080"},
			Dashboard: nexus.DashboardConfig{Enabled: true, Name: "myapp"},
		},
		helloModule,
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInitFrontend(dir, "vue", false, &out); err != nil {
		t.Fatalf("runInitFrontend: %v\nout: %s", err, out.String())
	}

	// Files written.
	for _, p := range []string{
		"web/src/main.ts",
		"web/src/App.vue",
		"web/index.html",
		"web/package.json",
		"web/vite.config.ts",
		"web/tsconfig.json",
		"web/sdk/nexus-vite-plugin.js",
		"web/sdk/nexus-vite-plugin.d.ts",
		"web/dist/index.html",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "web/viteless.config.ts")); err == nil {
		t.Error("nexus init must not write viteless.config.ts")
	}
	if p := inspectFrontend(filepath.Join(dir, "web")); !p.PackageJSON || p.Legacy != "" {
		t.Errorf("web/ should read as a Vite project: %+v", p)
	}
	if !strings.Contains(out.String(), "nexus dev") {
		t.Errorf("next steps should point at nexus dev:\n%s", out.String())
	}

	// main.go got the three pieces.
	body, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	patched := string(body)
	for _, want := range []string{
		`"embed"`,
		"//go:embed all:web/dist",
		"var webFS embed.FS",
		`nexus.ServeFrontend(webFS, "web/dist")`,
	} {
		if !strings.Contains(patched, want) {
			t.Errorf("main.go missing %q\n--- body ---\n%s", want, patched)
		}
	}
}

// TestInitFrontend_OnBootProject covers the `nexus new` default
// shape since v1.12.4: main.go calls nexus.Boot(...) with no
// nexus.Run, so the AST patcher must locate the Boot call to inject
// ServeFrontend. Regression guard for new → init.
func TestInitFrontend_OnBootProject(t *testing.T) {
	dir := t.TempDir()
	mainGo := `package main

import "github.com/paulmanoni/nexus"

func main() {
	nexus.Boot(
		helloModule,
	)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runInitFrontend(dir, "vue", false, &out); err != nil {
		t.Fatalf("runInitFrontend: %v\nout: %s", err, out.String())
	}

	body, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	patched := string(body)
	for _, want := range []string{
		"//go:embed all:web/dist",
		"var webFS embed.FS",
		`nexus.ServeFrontend(webFS, "web/dist")`,
		"nexus.Boot(",
	} {
		if !strings.Contains(patched, want) {
			t.Errorf("main.go missing %q\n--- body ---\n%s", want, patched)
		}
	}
}

// TestInitFrontend_Idempotent confirms re-running on a project
// that's already had nexus init --frontend doesn't double-write
// the islandsFS var or duplicate the ServeFrontend arg.
func TestInitFrontend_Idempotent(t *testing.T) {
	dir := t.TempDir()
	mainGo := `package main

import "github.com/paulmanoni/nexus"

func main() {
	nexus.Run(
		nexus.Config{Server: nexus.ServerConfig{Addr: ":8080"}},
		helloModule,
	)
}
`
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644)

	var out bytes.Buffer
	if err := runInitFrontend(dir, "react", true, &out); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Re-run with force; should NOT add ServeFrontend twice.
	out.Reset()
	if err := runInitFrontend(dir, "react", true, &out); err != nil {
		t.Fatalf("second run: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	if n := strings.Count(string(body), "nexus.ServeFrontend"); n != 1 {
		t.Errorf("ServeFrontend appears %d times after re-run, want 1\n%s", n, body)
	}
	if n := strings.Count(string(body), "var webFS embed.FS"); n != 1 {
		t.Errorf("webFS var appears %d times, want 1", n)
	}
}

// nexus init --frontend must ignore web/node_modules and web/dist/* (but
// not the stub) as nexus new does, or they get committed — creating the
// .gitignore when missing, appending to one that lacks the entries, and
// adding nothing on a re-run.
func TestInitFrontend_Gitignore(t *testing.T) {
	mainGo := "package main\n\nimport \"github.com/paulmanoni/nexus\"\n\nfunc main() {\n\tnexus.Boot()\n}\n"
	scaffolded, err := renderTemplate(".gitignore", tmplGitignoreTpl, scaffoldOpts{Frontend: "vue"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, before string }{
		{"missing", ""},
		{"no trailing newline", "/bin/\n*.log"},
		{"has only the dist rules", "/bin/\n/web/dist/*\n!/web/dist/index.html\n"},
		{"re-include before the dist rule", "!/web/dist/index.html\n/web/node_modules/\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestFile(t, filepath.Join(dir, "main.go"), mainGo)
			if c.before != "" {
				writeTestFile(t, filepath.Join(dir, ".gitignore"), c.before)
			}
			var out bytes.Buffer
			if err := runInitFrontend(dir, "vue", false, &out); err != nil {
				t.Fatalf("runInitFrontend: %v\n%s", err, out.String())
			}
			first, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
			gi := string(first)
			if !strings.HasPrefix(gi, c.before) {
				t.Errorf("existing entries not kept:\n%s", gi)
			}
			// What git would do: last matching rule wins.
			for path, ignored := range map[string]bool{
				"web/node_modules/":    true,
				"web/dist/assets/a.js": true,
				"web/dist/index.html":  false,
			} {
				if got := gitignoreIgnores(gi, path); got != ignored {
					t.Errorf("%s ignored = %v, want %v\n%s", path, got, ignored, gi)
				}
			}
			if strings.Contains(gi, "/web/sdk") {
				t.Errorf("web/sdk must not be ignored\n%s", gi)
			}
			if c.before == "" && gi != strings.TrimPrefix(gitignoreFrontend, "\n") {
				t.Errorf("new .gitignore = %q, want the frontend block", gi)
			}
			if !strings.Contains(scaffolded, gitignoreFrontend) {
				t.Errorf("nexus new's .gitignore no longer carries the same block")
			}

			if err := runInitFrontend(dir, "vue", true, &out); err != nil {
				t.Fatal(err)
			}
			if again, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(again) != gi {
				t.Errorf("re-run changed .gitignore:\n%s", again)
			}
		})
	}
}

// gitignoreIgnores applies the anchored patterns this test deals in, in
// order, the last match winning.
func gitignoreIgnores(gitignore, path string) bool {
	ignored := false
	for _, line := range strings.Split(gitignore, "\n") {
		line = strings.TrimSpace(line)
		neg := strings.HasPrefix(line, "!")
		pat := strings.TrimPrefix(strings.TrimPrefix(line, "!"), "/")
		if pat == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var match bool
		switch {
		case strings.HasSuffix(pat, "/*"):
			match = strings.HasPrefix(path, strings.TrimSuffix(pat, "*"))
		case strings.HasSuffix(pat, "/"):
			match = strings.HasPrefix(path, pat)
		default:
			match = path == pat
		}
		if match {
			ignored = !neg
		}
	}
	return ignored
}

// TestInitFrontend_NoMainGo surfaces a clear error when the
// target directory doesn't have a main.go — typical mistake of
// running from the wrong cwd.
func TestInitFrontend_NoMainGo(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runInitFrontend(dir, "vue", false, &out)
	if err == nil {
		t.Fatal("expected error on missing main.go")
	}
	if !strings.Contains(err.Error(), "no main.go") {
		t.Errorf("err missing 'no main.go' hint: %v", err)
	}
}

// TestInitFrontend_ExistingWebBlocksWithoutForce defends
// against accidental clobbering — user might have hand-written
// frontend bits before discovering the init flow.
func TestInitFrontend_ExistingWebBlocksWithoutForce(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "web"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "web", "App.vue"), []byte("<template>existing</template>"), 0o644)

	var out bytes.Buffer
	err := runInitFrontend(dir, "vue", false, &out)
	if err == nil {
		t.Fatal("expected error when web/ exists without --force")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("err missing --force guidance: %v", err)
	}
	// Existing file untouched.
	body, _ := os.ReadFile(filepath.Join(dir, "web", "App.vue"))
	if string(body) != "<template>existing</template>" {
		t.Errorf("user's existing App.vue clobbered: %s", body)
	}
}

// TestInitFrontend_BadFrontendValue catches a typo / unsupported
// framework name at the CLI surface rather than producing a
// broken scaffold halfway through.
func TestInitFrontend_BadFrontendValue(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	var out bytes.Buffer
	err := runInitFrontend(dir, "svelte", false, &out)
	if err == nil {
		t.Fatal("expected error on unknown frontend")
	}
	if !strings.Contains(err.Error(), "unknown frontend") {
		t.Errorf("err = %v", err)
	}
}

// TestInitFrontend_ForceMigratesLegacyWeb is the path the viteless
// migration hint names: a viteless-era web/ (sources, viteless.config.ts,
// no package.json) plus --force becomes a Vite project. The project files
// are written, the app's own sources are kept, and the stale viteless
// config is pointed out rather than deleted.
func TestInitFrontend_ForceMigratesLegacyWeb(t *testing.T) {
	dir := t.TempDir()
	mainGo := "package main\n\nimport \"github.com/paulmanoni/nexus\"\n\nfunc main() {\n\tnexus.Boot()\n}\n"
	_ = os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainGo), 0o644)
	web := filepath.Join(dir, "web")
	_ = os.MkdirAll(filepath.Join(web, "src"), 0o755)
	legacy := map[string]string{
		"viteless.config.ts": "import { defineConfig } from 'viteless'\nexport default defineConfig({})\n",
		"index.html":         "<!doctype html><title>mine</title><div id=\"app\"></div>\n",
		"src/App.vue":        "<template>mine</template>\n",
		"tsconfig.json":      `{"include": ["src", "viteless-env.d.ts"]}`,
	}
	for rel, body := range legacy {
		_ = os.WriteFile(filepath.Join(web, rel), []byte(body), 0o644)
	}
	if p := inspectFrontend(web); p.Legacy == "" {
		t.Fatalf("fixture should read as a legacy viteless dir: %+v", p)
	}

	var out bytes.Buffer
	if err := runInitFrontend(dir, "vue", true, &out); err != nil {
		t.Fatalf("runInitFrontend --force: %v\n%s", err, out.String())
	}
	for _, rel := range []string{"index.html", "src/App.vue"} {
		if b, _ := os.ReadFile(filepath.Join(web, rel)); string(b) != legacy[rel] {
			t.Errorf("web/%s was overwritten: %s", rel, b)
		}
	}
	if ts, _ := os.ReadFile(filepath.Join(web, "tsconfig.json")); strings.Contains(string(ts), "viteless") {
		t.Errorf("tsconfig.json should be replaced: %s", ts)
	}
	for _, rel := range []string{"package.json", "vite.config.ts", "sdk/nexus-vite-plugin.js", "src/main.ts"} {
		if _, err := os.Stat(filepath.Join(web, rel)); err != nil {
			t.Errorf("missing web/%s: %v", rel, err)
		}
	}
	if p := inspectFrontend(web); !p.PackageJSON || p.Legacy != "" {
		t.Errorf("web/ should now read as a Vite project: %+v", p)
	}
	for _, want := range []string{"kept  web/index.html", "kept  web/src/App.vue", "web/viteless.config.ts is no longer read"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// TestInitFrontend_OnScaffoldedMain patches the main.go nexus new writes
// for an app with no frontend. That file has comments, and a directive
// added to the AST as a position-less comment was dropped by the printer,
// leaving "var webFS embed.FS" with no //go:embed: an app that compiled
// and served an empty frontend.
func TestInitFrontend_OnScaffoldedMain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plain")
	if err := scaffoldWithOpts(scaffoldOpts{
		Dir: dir, Frontend: "none", DB: "none", Cache: "none", Auth: "none",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	var out bytes.Buffer
	if err := runInitFrontend(dir, "vue", false, &out); err != nil {
		t.Fatalf("runInitFrontend: %v\n%s", err, out.String())
	}
	body, _ := os.ReadFile(filepath.Join(dir, "main.go"))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", body, parser.ParseComments)
	if err != nil {
		t.Fatalf("patched main.go does not parse: %v\n%s", err, body)
	}
	var directive bool
	for _, decl := range file.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || gd.Doc == nil || !hasVarDecl(&ast.File{Decls: []ast.Decl{gd}}, "webFS") {
			continue
		}
		for _, c := range gd.Doc.List {
			directive = directive || c.Text == "//go:embed all:web/dist"
		}
	}
	if !directive {
		t.Fatalf("var webFS lost its //go:embed directive:\n%s", body)
	}
	for _, want := range []string{"nexus.ServeFrontend(webFS, \"web/dist\")", "// nexus.Boot loads nexus.toml"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("main.go missing %q:\n%s", want, body)
		}
	}
}
