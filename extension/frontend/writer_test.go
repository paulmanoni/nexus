package frontend

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/paulmanoni/nexus"
)

func TestWrite_CreatesFiles(t *testing.T) {
	dir := t.TempDir()
	files := []nexus.GeneratedFile{
		{Path: "_client.ts", Body: []byte("// client")},
		{Path: "types.ts", Body: []byte("// types")},
		{Path: "sub/nested.ts", Body: []byte("// nested")},
	}
	changed, unchanged, err := Write(dir, files, io.Discard)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if changed != 3 || unchanged != 0 {
		t.Fatalf("changed=%d unchanged=%d, want 3/0", changed, unchanged)
	}
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(dir, f.Path))
		if err != nil {
			t.Fatalf("read %s: %v", f.Path, err)
		}
		if string(body) != string(f.Body) {
			t.Fatalf("body mismatch for %s", f.Path)
		}
	}
}

// TestWrite_PreservesMtimeOnNoOp asserts the byteEqual short-circuit:
// a second Write with identical bytes shouldn't bump the file's mtime,
// because IDE file watchers reindex on mtime change and a no-op
// `nexus generate` shouldn't cost an editor reload.
func TestWrite_PreservesMtimeOnNoOp(t *testing.T) {
	dir := t.TempDir()
	files := []nexus.GeneratedFile{
		{Path: "index.ts", Body: []byte("// hello")},
	}
	if _, _, err := Write(dir, files, io.Discard); err != nil {
		t.Fatal(err)
	}
	info1, err := os.Stat(filepath.Join(dir, "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	changed, unchanged, err := Write(dir, files, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 0 || unchanged != 1 {
		t.Fatalf("no-op pass: changed=%d unchanged=%d, want 0/1", changed, unchanged)
	}
	info2, err := os.Stat(filepath.Join(dir, "index.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Fatalf("mtime changed across no-op: %v → %v", info1.ModTime(), info2.ModTime())
	}
}

func TestWrite_RewritesOnChange(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Write(dir, []nexus.GeneratedFile{{Path: "x.ts", Body: []byte("v1")}}, io.Discard); err != nil {
		t.Fatal(err)
	}
	changed, _, err := Write(dir, []nexus.GeneratedFile{{Path: "x.ts", Body: []byte("v2")}}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed=%d, want 1", changed)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "x.ts"))
	if string(body) != "v2" {
		t.Fatalf("body=%q, want v2", body)
	}
}

// TestWrite_RejectsUnsafePath blocks two foot-guns: absolute paths
// (which would write outside outDir) and "../" sequences (which would
// escape the directory tree). The driver should never produce these,
// but defending the writer is cheap.
func TestWrite_RejectsUnsafePath(t *testing.T) {
	dir := t.TempDir()
	cases := []string{"/etc/passwd", "../escape.ts", "ok/../../bad.ts"}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			_, _, err := Write(dir, []nexus.GeneratedFile{{Path: p, Body: []byte("x")}}, io.Discard)
			if err == nil {
				t.Fatalf("Write accepted unsafe path %q", p)
			}
		})
	}
}
