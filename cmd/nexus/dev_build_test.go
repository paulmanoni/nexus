package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTinyModule lays down a one-file main package that binds nothing and
// exits immediately, so the build tests stay fast and side-effect free.
func writeTinyModule(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("go.mod", "module devbuildtest\n\ngo 1.22\n")
	write("main.go", body)
	return dir
}

const tinyMain = `package main

import "fmt"

func main() { fmt.Println("ok") }
`

func TestDevBuilderBuildsAndIsDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	dir := writeTinyModule(t, tinyMain)

	b, err := newDevBuilder(false)
	if err != nil {
		t.Fatalf("newDevBuilder: %v", err)
	}
	defer b.close()

	first, err := b.build(context.Background(), dir, "", io.Discard)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := os.Stat(first); err != nil {
		t.Fatalf("built binary missing: %v", err)
	}

	second, err := b.build(context.Background(), dir, "", io.Discard)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if first == second {
		t.Fatalf("consecutive builds reused path %s; the running child still holds the old file", first)
	}

	// Same sources → identical bytes. This is what lets the dev loop skip
	// the restart when a save didn't reach the app's build graph.
	h1, err := fileHash(first)
	if err != nil {
		t.Fatalf("hash first: %v", err)
	}
	h2, err := fileHash(second)
	if err != nil {
		t.Fatalf("hash second: %v", err)
	}
	if h1 != h2 {
		t.Errorf("unchanged sources produced different binaries:\n  %s\n  %s", h1, h2)
	}

	// A real source change must produce different bytes, or the loop would
	// skip a restart the user needs.
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte(strings.Replace(tinyMain, `"ok"`, `"changed"`, 1)), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	third, err := b.build(context.Background(), dir, "", io.Discard)
	if err != nil {
		t.Fatalf("build after edit: %v", err)
	}
	h3, err := fileHash(third)
	if err != nil {
		t.Fatalf("hash third: %v", err)
	}
	if h3 == h1 {
		t.Error("edited source produced an identical binary; the restart would be skipped")
	}
}

func TestDevBuilderReportsCompileErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	dir := writeTinyModule(t, "package main\n\nfunc main() { var x int = \"nope\"; _ = x }\n")

	b, err := newDevBuilder(false)
	if err != nil {
		t.Fatalf("newDevBuilder: %v", err)
	}
	defer b.close()

	var out strings.Builder
	bin, err := b.build(context.Background(), dir, "", &out)
	if err == nil {
		t.Fatal("expected a build error")
	}
	if bin != "" {
		t.Errorf("failed build returned a path: %q", bin)
	}
	if !strings.Contains(out.String(), "nope") {
		t.Errorf("compiler diagnostics not forwarded, got: %q", out.String())
	}
}

func TestDevBuilderCloseRemovesBinaries(t *testing.T) {
	b, err := newDevBuilder(false)
	if err != nil {
		t.Fatalf("newDevBuilder: %v", err)
	}
	dir := b.dir
	b.close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("build dir %s survived close: %v", dir, err)
	}
}

// The prewarm exec must abort in the Go runtime — before package inits and
// main — so it never binds a port or touches the app's resources.
func TestDevBuilderPrewarmDoesNotRunMain(t *testing.T) {
	if testing.Short() {
		t.Skip("invokes the go toolchain")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	dir := writeTinyModule(t, `package main

import "os"

func init() { os.WriteFile("`+marker+`", []byte("init"), 0o644) }

func main() { os.WriteFile("`+marker+`", []byte("main"), 0o644) }
`)

	b, err := newDevBuilder(false)
	if err != nil {
		t.Fatalf("newDevBuilder: %v", err)
	}
	defer b.close()

	bin, err := b.build(context.Background(), dir, "", io.Discard)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	done := make(chan struct{})
	go func() { b.prewarm(context.Background(), bin); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("prewarm did not return")
	}

	if _, err := os.Stat(marker); err == nil {
		t.Error("prewarm executed the app's init/main; it must abort during runtime startup")
	}

	// And the point of it: the real exec afterwards is the cheap one.
	if _, err := os.Stat(bin); err != nil {
		t.Errorf("prewarm removed the binary: %v", err)
	}
}
