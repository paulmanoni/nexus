package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

// fakeOutputs builds an []api.OutputFile mimicking what esbuild
// produces for an entry-by-name list. Lets the tests skip the
// real bundler.
func fakeOutputs(names ...string) []api.OutputFile {
	out := make([]api.OutputFile, len(names))
	for i, n := range names {
		out[i] = api.OutputFile{Path: filepath.Join("/tmp/out", n)}
	}
	return out
}

func TestRewriteIndexHTML_VueViteShape(t *testing.T) {
	// The exact shape the user's nexus-cloud project ships:
	// Vite-style index.html at islands.src/, entry at /src/main.ts.
	source := []byte(`<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
    <link rel="preconnect" href="https://fonts.googleapis.com" />
    <link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Inter" />
    <title>Nexus Cloud</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.ts"></script>
  </body>
</html>`)
	out := rewriteIndexHTML(source, fakeOutputs("main.js", "main.css", "main.js.map", "main.css.map"))
	s := string(out)

	if !strings.Contains(s, `<script type="module" src="/main.js"></script>`) {
		t.Errorf("entry script not rewritten:\n%s", s)
	}
	if strings.Contains(s, "/src/main.ts") {
		t.Errorf("source ref still present:\n%s", s)
	}
	// Sidecar CSS auto-injected.
	if !strings.Contains(s, `<link rel="stylesheet" href="/main.css">`) {
		t.Errorf("sidecar CSS link missing:\n%s", s)
	}
	// External fonts CSS preserved verbatim.
	if !strings.Contains(s, `href="https://fonts.googleapis.com/css2?family=Inter"`) {
		t.Errorf("external CSS link mangled:\n%s", s)
	}
	// favicon untouched.
	if !strings.Contains(s, `href="/favicon.svg"`) {
		t.Errorf("favicon ref mangled:\n%s", s)
	}
	// preconnect untouched.
	if !strings.Contains(s, `href="https://fonts.googleapis.com"`) {
		t.Errorf("preconnect mangled:\n%s", s)
	}
}

func TestRewriteIndexHTML_ExistingStylesheetIsRemapped(t *testing.T) {
	source := []byte(`<head>
<link rel="stylesheet" href="/src/styles/main.css">
</head>
<body><script type="module" src="/src/main.ts"></script></body>`)
	out := rewriteIndexHTML(source, fakeOutputs("main.js", "main.css"))
	s := string(out)
	if !strings.Contains(s, `<link rel="stylesheet" href="/main.css">`) {
		t.Errorf("css link not rewritten:\n%s", s)
	}
	// Once rewritten, we should NOT have injected a duplicate
	// link (the rewrite covered it).
	links := strings.Count(s, `href="/main.css"`)
	if links != 1 {
		t.Errorf("expected 1 main.css link, got %d:\n%s", links, s)
	}
}

func TestRewriteIndexHTML_NoMatchingOutputLeavesUntouched(t *testing.T) {
	// Source references a script the bundler didn't produce (typo
	// in the user's index.html). Leave it alone — the bundler's
	// own logs already complain, and silently rewriting to "" or
	// the wrong file would make debugging harder.
	source := []byte(`<script type="module" src="/src/typo.ts"></script>`)
	out := rewriteIndexHTML(source, fakeOutputs("main.js"))
	if !strings.Contains(string(out), `src="/src/typo.ts"`) {
		t.Errorf("unmatched src should pass through, got: %s", out)
	}
}

func TestRewriteIndexHTML_MultipleEntriesEachMapped(t *testing.T) {
	source := []byte(`<head><script type="module" src="/src/main.ts"></script>
<script type="module" src="/src/admin.ts"></script></head>`)
	out := rewriteIndexHTML(source, fakeOutputs("main.js", "admin.js"))
	s := string(out)
	if !strings.Contains(s, `src="/main.js"`) {
		t.Errorf("main not rewritten:\n%s", s)
	}
	if !strings.Contains(s, `src="/admin.js"`) {
		t.Errorf("admin not rewritten:\n%s", s)
	}
}

func TestRewriteIndexHTML_NoEntryOnlyLinkRefs(t *testing.T) {
	// A pure-CSS page (no script) should still get its <link>
	// rewritten when the bundler produced a matching .css.
	source := []byte(`<head><link rel="stylesheet" href="/src/global.css"></head>`)
	out := rewriteIndexHTML(source, fakeOutputs("global.css"))
	if !strings.Contains(string(out), `href="/global.css"`) {
		t.Errorf("css-only rewrite failed:\n%s", out)
	}
}

func TestEmitIndexHTML_FindsIndexOneLevelUp(t *testing.T) {
	dir := t.TempDir()
	srcSub := filepath.Join(dir, "src")
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(srcSub, 0o755); err != nil {
		t.Fatal(err)
	}
	// index.html at the parent of the entry folder — the Vite
	// shape that broke nexus dev for the user.
	indexBody := []byte(`<head><script type="module" src="/src/main.ts"></script></head>`)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), indexBody, 0o644); err != nil {
		t.Fatal(err)
	}

	err := emitIndexHTML(srcSub, outDir, fakeOutputs("main.js"), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(outDir, "index.html"))
	if err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if !strings.Contains(string(body), `src="/main.js"`) {
		t.Errorf("rewritten ref missing:\n%s", body)
	}
}

func TestEmitIndexHTML_NoSourceIsNoop(t *testing.T) {
	// No index.html anywhere: emit returns nil + writes nothing.
	dir := t.TempDir()
	srcSub := filepath.Join(dir, "src")
	outDir := filepath.Join(dir, "out")
	_ = os.MkdirAll(srcSub, 0o755)

	if err := emitIndexHTML(srcSub, outDir, fakeOutputs("main.js"), &bytes.Buffer{}); err != nil {
		t.Errorf("missing source should be no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "index.html")); !os.IsNotExist(err) {
		t.Error("output should not have been created when no source index.html exists")
	}
}

// TestPluralize_BasicPlurals: the rules that fix "2 entrys" in
// the watcher output. Includes the canonical y→ies, sibling
// suffixes (x/z/sh/ch), and the "boys" vowel-y exception.
func TestPluralize_BasicPlurals(t *testing.T) {
	cases := []struct {
		word string
		n    int
		want string
	}{
		{"entry", 1, "entry"},
		{"entry", 2, "entries"},
		{"file", 2, "files"},
		{"box", 2, "boxes"},
		{"bus", 2, "buses"},
		{"church", 2, "churches"},
		{"dish", 2, "dishes"},
		{"boy", 2, "boys"},   // vowel + y → just add s
		{"day", 2, "days"},
		// Suffix-only form — pluralize("", n) returns just "s"
		// so callers using `"%d route%s"` get "routes" / "route".
		{"", 2, "s"},
		{"", 1, ""},
	}
	for _, c := range cases {
		got := pluralize(c.word, c.n)
		if got != c.want {
			t.Errorf("pluralize(%q, %d) = %q, want %q", c.word, c.n, got, c.want)
		}
	}
}
