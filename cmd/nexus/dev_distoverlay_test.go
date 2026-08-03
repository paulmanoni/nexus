package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDistStubReplacements(t *testing.T) {
	dir := t.TempDir()
	dist := filepath.Join(dir, "web", "dist")
	writeFiles(t, dist, map[string]string{
		"index.html":            "<html>real</html>",
		"assets/app-abc123.js":  "console.log('a lot of bundled javascript')",
		"assets/app-abc123.css": "body{}",
		"nested/index.html":     "<html>prerendered</html>",
		".vite/manifest.json":   `{"src/main.ts":{"file":"assets/app-abc123.js"}}`,
	})
	tmp := t.TempDir()

	rep, err := distStubReplacements(dist, tmp)
	if err != nil {
		t.Fatalf("distStubReplacements: %v", err)
	}

	// The Vite manifest stays real — extension/inertia reads it through the
	// embed when NEXUS_VITE_DEV isn't set.
	if _, stubbed := rep[filepath.Join(dist, ".vite", "manifest.json")]; stubbed {
		t.Error("manifest.json was stubbed; Inertia resolves entry chunks from it")
	}
	// Everything else is replaced.
	for _, rel := range []string{"index.html", "assets/app-abc123.js", "assets/app-abc123.css", "nested/index.html"} {
		if _, ok := rep[filepath.Join(dist, rel)]; !ok {
			t.Errorf("%s was not stubbed", rel)
		}
	}
	// The bundle's own index.html keeps real HTML (boot fails fast without
	// it); a nested one is just an asset.
	top, err := os.ReadFile(rep[filepath.Join(dist, "index.html")])
	if err != nil {
		t.Fatal(err)
	}
	if len(top) == 0 {
		t.Error("top-level index.html stub is empty; ServeFrontend needs it to parse")
	}
	nested, err := os.ReadFile(rep[filepath.Join(dist, "nested", "index.html")])
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 0 {
		t.Errorf("nested index.html stub = %q, want empty", nested)
	}
}

func TestDistStubReplacementsAbsentOrEmpty(t *testing.T) {
	tmp := t.TempDir()
	for _, root := range []string{"", filepath.Join(t.TempDir(), "nope"), t.TempDir()} {
		rep, err := distStubReplacements(root, tmp)
		if err != nil {
			t.Fatalf("distStubReplacements(%q): %v", root, err)
		}
		if rep != nil {
			t.Errorf("distStubReplacements(%q) = %v, want nil", root, rep)
		}
	}
}

// The dist stubs and the handler-codegen artifacts have to share one overlay
// document, since `go build` takes a single -overlay file.
func TestBuildDevOverlayMergesDistStubs(t *testing.T) {
	dir := t.TempDir()
	dist := filepath.Join(dir, "web", "dist")
	writeFiles(t, dist, map[string]string{"index.html": "<html>real</html>", "assets/x.js": "x"})
	writeFiles(t, dir, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})

	path, cleanup, err := buildDevOverlay(dir, dist)
	if err != nil {
		t.Fatalf("buildDevOverlay: %v", err)
	}
	defer cleanup()
	if path == "" {
		t.Fatal("no overlay written even though the bundle has files to stub")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Replace map[string]string }
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("overlay is not valid JSON: %v", err)
	}
	if _, ok := doc.Replace[filepath.Join(dist, "assets", "x.js")]; !ok {
		t.Errorf("overlay is missing the bundle stubs: %v", doc.Replace)
	}
}

// nexus build must never stub the bundle — it produces the real binary.
func TestBuildDevOverlayNoStubRootIsNoop(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"main.go":          "package main\n\nfunc main() {}\n",
		"web/dist/x.js":    "x",
		"web/dist/idx.txt": "y",
	})
	path, cleanup, err := buildDevOverlay(dir, "")
	if err != nil {
		t.Fatalf("buildDevOverlay: %v", err)
	}
	defer cleanup()
	if path != "" {
		t.Errorf("overlay = %q, want none when no annotations and no stub root", path)
	}
}

func TestDetectServeFrontendRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{"serve frontend", "package main\n\nfunc main() { nexus.Boot(nexus.ServeFrontend(webFS, \"web/dist\")) }\n", "web/dist"},
		{"custom root", "package main\n\nfunc main() { nexus.Boot(nexus.ServeFrontend(webFS, \"ui/build\")) }\n", "ui/build"},
		{"no call", "package main\n\nfunc main() {}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.go": tc.src})
			if got := detectServeFrontendRoot(dir); got != tc.want {
				t.Errorf("detectServeFrontendRoot = %q, want %q", got, tc.want)
			}
		})
	}
}
