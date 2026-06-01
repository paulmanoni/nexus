package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitFrontend_OnGoOnlyProject is the headline path: an
// existing Go project with no frontend gets the pipeline added
// in one command. Asserts the islands.src/ + islands/ tree
// drops in AND that main.go gets patched correctly.
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
		"web/vite.config.ts",
		"web/dist/index.html",
	} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
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

// TestInitFrontend_ExistingIslandsBlocksWithoutForce defends
// against accidental clobbering — user might have hand-written
// frontend bits before discovering the init flow.
func TestInitFrontend_ExistingIslandsBlocksWithoutForce(t *testing.T) {
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
