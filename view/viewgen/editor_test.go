package viewgen

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// An editor's unsaved .templ buffer replaces the file on disk, Editor mode
// keeps templ's raw output with its source map, and a templ syntax error
// is a PositionError.
func TestGenerateWithEditorSources(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"go.mod":           "module example.com/app\n\ngo 1.26\n",
		"pages/home.templ": "package pages\n\ntempl Home() {\n\t<p>disk</p>\n}\n",
	})
	home := filepath.Join(root, "pages", "home.templ")
	buffer := "package pages\n\ntempl Home(name string) {\n\t<p>{ name }</p>\n}\n"
	plan, err := GenerateWith(root, Options{Sources: map[string][]byte{home: []byte(buffer)}, Editor: true})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "pages", "home_templ.go")
	if !strings.Contains(string(plan.Files[out]), "func Home(name string)") {
		t.Fatalf("the buffer was not compiled:\n%s", plan.Files[out])
	}
	m := plan.Maps[out]
	if m == nil || m.Templ != home {
		t.Fatalf("Maps[%s] = %+v", out, m)
	}
	// { name } is at line 3 (0-based), col 6.
	tgt, ok := m.Map.TargetPositionFromSource(3, 6)
	if !ok {
		t.Fatal("no target for { name }")
	}
	lines := strings.Split(string(plan.Files[out]), "\n")
	if got := lines[tgt.Line][tgt.Col:]; !strings.HasPrefix(got, "name") {
		t.Fatalf("target %d:%d is %q, want name", tgt.Line, tgt.Col, got)
	}

	// A buffer for a file not yet on disk is compiled too.
	extra := filepath.Join(root, "pages", "about.templ")
	plan, err = GenerateWith(root, Options{Sources: map[string][]byte{extra: []byte("package pages\n\ntempl About() {\n\t<p>about</p>\n}\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.Files[filepath.Join(root, "pages", "about_templ.go")]; !ok {
		t.Fatal("an unsaved new .templ file was not compiled")
	}

	_, err = GenerateWith(root, Options{Sources: map[string][]byte{home: []byte("package pages\n\ntempl Home() {\n\t<p>{ </p>\n}\n")}})
	var pe *PositionError
	if !errors.As(err, &pe) || pe.File != filepath.ToSlash(home) || pe.Line < 1 {
		t.Fatalf("err = %v, want a PositionError in home.templ", err)
	}
	t.Logf("syntax error: %v", pe)
}
