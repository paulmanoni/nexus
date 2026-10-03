package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestViewsGeneratorWatches(t *testing.T) {
	g := viewsGenerator(t.TempDir())
	for path, want := range map[string]bool{
		"/app/pages/home.templ":         true,
		"/app/state/search.go":          true, // may declare a state struct
		"/app/pages/home_templ.go":      false,
		"/app/pages/view_gen.go":        false,
		"/app/view_imports_gen.go":      false,
		"/app/pages/home_test.go":       false,
		"/app/web/src/App.vue":          false,
		"/app/pages/home.templ.swp.tmp": false,
	} {
		if got := g.watches(path); got != want {
			t.Errorf("watches(%s) = %v, want %v", path, got, want)
		}
	}
}

func TestDevGeneratorsNeedTemplates(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
	if len(devGenerators(dir)) != 0 {
		t.Fatal("no .templ files, no generators")
	}
	writeFile(t, filepath.Join(dir, "pages", "home.templ"), "package pages\n")
	if gens := devGenerators(dir); len(gens) != 1 || gens[0].name != "views" {
		t.Fatalf("gens = %+v", gens)
	}
}

// A generator runs once per burst of changes, never twice at once, and a
// change during a run schedules exactly one more.
func TestGeneratorRunnerCoalesces(t *testing.T) {
	var runs, active, overlap atomic.Int32
	g := devGenerator{
		name:    "t",
		watches: func(p string) bool { return strings.HasSuffix(p, ".templ") },
		run: func() (string, error) {
			if active.Add(1) > 1 {
				overlap.Store(1)
			}
			runs.Add(1)
			time.Sleep(150 * time.Millisecond)
			active.Add(-1)
			return "1 file(s) updated", nil
		},
	}
	var out lockedBuffer
	r := newGeneratorRunner(context.Background(), []devGenerator{g}, &out)
	r.debounce = 20 * time.Millisecond

	r.runAll()
	for i := 0; i < 5; i++ { // one burst
		r.changed("/x/a.templ")
	}
	r.changed("/x/a.go") // not watched
	time.Sleep(80 * time.Millisecond)
	r.changed("/x/b.templ") // during the burst's run
	time.Sleep(60 * time.Millisecond)
	r.wait()
	time.Sleep(50 * time.Millisecond)
	r.wait()

	if n := runs.Load(); n != 3 {
		t.Fatalf("%d runs, want 3 (startup, the burst, one rerun):\n%s", n, out.String())
	}
	if overlap.Load() != 0 {
		t.Fatal("a generator ran twice at once")
	}
	if !strings.Contains(out.String(), "t · 1 file(s) updated") {
		t.Fatalf("output:\n%s", out.String())
	}
}

func TestGeneratorFailureIsReported(t *testing.T) {
	var out lockedBuffer
	g := devGenerator{name: "views", watches: func(string) bool { return false }, run: func() (string, error) {
		return "", errors.New("pages/home.templ:12:5: this text reads a signal, so it runs in the browser: the / operator is not supported\npets/search.templ:3:1: second")
	}}
	newGeneratorRunner(context.Background(), []devGenerator{g}, &out).runAll()
	s := out.String()
	if !strings.Contains(s, "views failed") || !strings.Contains(s, "[views]\x1b[0m pages/home.templ:12:5:") || !strings.Contains(s, "pets/search.templ:3:1") {
		t.Fatalf("output:\n%s", s)
	}
}

// The watcher tells onChange about every edited file, build input or not.
func TestWatchSourceReportsEdits(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module x\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan string, 8)
	if err := watchSource(ctx, dir, make(chan struct{}, 1), &bytes.Buffer{}, nil, func(p string) { seen <- p }); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, ".hidden.templ"), "x")
	writeFile(t, filepath.Join(dir, "page.templ"), "x")
	select {
	case p := <-seen:
		if filepath.Base(p) != "page.templ" {
			t.Fatalf("saw %s first; hidden files must be skipped", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the edit of page.templ was not reported")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *lockedBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *lockedBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A view's //nexus:page, copied by templ into its generated *_templ.go, is not a
// handler annotation: handler codegen ignores it.
func TestHandlerScanSkipsTemplOutput(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "pages", "home_templ.go"), `package pages

//nexus:page GET /
func Home() {}
`)
	results, err := scanHandlerSites(dir, handlerGenFileName)
	if err != nil {
		t.Fatalf("a view's //nexus:page broke handler codegen: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d generated files, want none", len(results))
	}
}
