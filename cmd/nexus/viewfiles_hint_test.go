package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestViewFilesIgnoreHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n")
	write("page.templ", "package main\n\ntempl Page() {\n\t<p>hi</p>\n}\n")
	if h := viewFilesIgnoreHint(dir); h != "" {
		t.Fatalf("outside a git work tree: %q", h)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	h := viewFilesIgnoreHint(dir)
	for _, want := range []string{"*_templ.go", "view_gen.go", "view_imports_gen.go"} {
		if !strings.Contains(h, want) {
			t.Fatalf("hint %q lacks %s", h, want)
		}
	}
	write(".gitignore", "*_templ.go\nview_gen.go\nview_imports_gen.go\n")
	if h := viewFilesIgnoreHint(dir); h != "" {
		t.Fatalf("all ignored: %q", h)
	}
}
