package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/paulmanoni/nexus/v2/view/viewgen"
)

func TestAddUI(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/shop\n\ngo 1.26\n")
	target := filepath.Join(dir, "internal", "kit")

	res, err := addUI(target, "", []string{"DataTable"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range res.wrote {
		got = append(got, filepath.Base(f))
	}
	sort.Strings(got)
	want := "base.go button.templ field.templ icon.templ menu.templ table.templ ui.css ui.go ui.js"
	if strings.Join(got, " ") != want {
		t.Fatalf("wrote %v, want %s", got, want)
	}
	if res.pkg != "kit" || res.importPath != "example.com/shop/internal/kit" {
		t.Fatalf("package %q, import %q", res.pkg, res.importPath)
	}
	for _, f := range []string{"table.templ", "base.go", "ui.go"} {
		b, _ := os.ReadFile(filepath.Join(target, f))
		if !bytes.HasPrefix(skipComments(b), []byte("package kit\n\n// Copied from")) || bytes.Contains(b, []byte("\npackage ui")) {
			t.Errorf("%s: package clause not rewritten:\n%s", f, b[:min(len(b), 300)])
		}
	}
	if b, _ := os.ReadFile(filepath.Join(target, "ui.go")); !bytes.Contains(b, []byte(`const Prefix = "/_ui/kit/"`)) {
		t.Error("ui.go keeps the kit's asset prefix")
	}
	if b, _ := os.ReadFile(filepath.Join(target, "ui.js")); !bytes.HasPrefix(b, []byte("/* Copied from")) {
		t.Error("ui.js lacks the vendored note")
	}

	// A second run keeps what is there (and what the app changed).
	writeFile(t, filepath.Join(target, "button.templ"), "package kit\n// mine\n")
	res, err = addUI(target, "", []string{"button", "badge"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.wrote) != 1 || filepath.Base(res.wrote[0]) != "badge.templ" {
		t.Fatalf("second run wrote %v", res.wrote)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "button.templ")); string(b) != "package kit\n// mine\n" {
		t.Fatal("an existing file was overwritten without --force")
	}
	if _, err := addUI(target, "", []string{"button"}, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "button.templ")); strings.Contains(string(b), "// mine") {
		t.Fatal("--force did not overwrite")
	}

	if _, err := addUI(target, "", []string{"carousel"}, false); err == nil || !strings.Contains(err.Error(), "no ui component") {
		t.Fatalf("unknown component: %v", err)
	}
	if _, err := addUI(filepath.Join(dir, "my-ui"), "", []string{"badge"}, false); err != nil {
		t.Fatalf("a dashed directory: %v", err)
	}
	if _, err := addUI(filepath.Join(dir, "9x"), "", []string{"badge"}, false); err == nil {
		t.Fatal("an invalid package name was accepted")
	}
}

func skipComments(b []byte) []byte {
	for bytes.HasPrefix(b, []byte("//")) {
		i := bytes.IndexByte(b, '\n')
		b = b[i+1:]
	}
	return b
}

// Every component, vendored, compiles as the app's own package.
func TestAddUICompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a module")
	}
	_, file, _, _ := runtime.Caller(0)
	repo, _ := filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/shop\n\ngo 1.26.2\n\nrequire github.com/paulmanoni/nexus/v2 v2.0.0\n\nreplace github.com/paulmanoni/nexus/v2 v2.0.0 => "+repo+"\n")
	sum, err := os.ReadFile(filepath.Join(repo, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(sum))
	if _, err := addUI(filepath.Join(dir, "ui"), "", []string{"all"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := viewgen.Module(dir); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}
