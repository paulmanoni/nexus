package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// nexus test compiles views that exist only as .templ; plain go test
// cannot.
func TestGoToolOverlaysViews(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"go.mod":            "module example.com/app\n\ngo 1.26\n\nrequire github.com/a-h/templ v0.3.1020\n",
		"views/hello.templ": helloTempl,
		"views/hello_test.go": `package views

import (
	"context"
	"strings"
	"testing"
)

func TestHello(t *testing.T) {
	var b strings.Builder
	if err := Hello("pets").Render(context.Background(), &b); err != nil || !strings.Contains(b.String(), "Hello, pets") {
		t.Fatalf("%q %v", b.String(), err)
	}
}
`,
	}
	for name, src := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")
	if out, err := exec.Command("go", "mod", "download", "github.com/a-h/templ").CombinedOutput(); err != nil {
		t.Skipf("templ is not in the module cache: %v\n%s", err, out)
	}

	if err := exec.Command("go", "test", "./...").Run(); err == nil {
		t.Fatal("plain go test passed without the generated views")
	}
	var out bytes.Buffer
	if err := runGoTool("test", []string{"./..."}, &out, &out); err != nil {
		t.Fatalf("nexus test: %v\n%s", err, &out)
	}
	if err := runGoTool("vet", []string{"./..."}, &out, &out); err != nil {
		t.Fatalf("nexus vet: %v\n%s", err, &out)
	}
	if _, err := os.Stat(filepath.Join(root, "views", "hello_templ.go")); err == nil {
		t.Fatal("nexus test wrote generated Go into the tree")
	}
}
