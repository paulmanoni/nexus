package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// [runtime.tailwind] imports become @imports of the module's files where
// the module sits, conditions kept, ahead of the @source lines.
func TestTailwindImports(t *testing.T) {
	lib := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lib, "css", "styles"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"css/base.css", "css/styles/nova.css"} {
		if err := os.WriteFile(filepath.Join(lib, filepath.FromSlash(f)), []byte("/* */"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mods := map[string]string{
		"example.com/ui":    t.TempDir(),
		"example.com/ui/v2": lib,
	}
	lines, err := resolveTailwindImports([]string{
		"example.com/ui/v2/css/base.css",
		"example.com/ui/v2/css/styles/nova.css layer(base)",
	}, mods)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`@import "` + filepath.ToSlash(filepath.Join(lib, "css", "base.css")) + `";`,
		`@import "` + filepath.ToSlash(filepath.Join(lib, "css", "styles", "nova.css")) + `" layer(base);`,
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	if _, err := resolveTailwindImports([]string{"example.com/other/x.css"}, mods); err == nil || !strings.Contains(err.Error(), "no module the project depends on") {
		t.Errorf("a file of no dependency: %v", err)
	}
	if _, err := resolveTailwindImports([]string{"example.com/ui/v2/css/missing.css"}, mods); err == nil {
		t.Error("a missing file is an error")
	}

	root := t.TempDir()
	e := tailwindEntry{Sources: filepath.Join(root, "assets", tailwindSourcesFile)}
	if err := os.MkdirAll(filepath.Dir(e.Sources), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeTailwindSources(root, e, nil, lines); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(e.Sources)
	if got := string(b); strings.Index(got, "@import") > strings.Index(got, "@source") || !strings.Contains(got, want[1]) {
		t.Errorf("sources.generated.css:\n%s", got)
	}
}
