package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchSource_FiresOnGoFileChange verifies the watcher emits a
// rebuild signal when a .go file under the watched root is written.
// This is the load-bearing case — every Cmd-S on a Go source file
// must trigger a restart.
func TestWatchSource_FiresOnGoFileChange(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}

	// Give the watcher a moment to register inotify on the dir before
	// the first edit — fsnotify's Add is sync but the goroutine that
	// drains events isn't yet scheduled.
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(src, []byte("package main\n// edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("expected restart signal after .go file write")
	}
}

// TestWatchSource_IgnoresIrrelevantPaths verifies the watcher does
// NOT fire for hidden dotfiles (editor buffer state) or skipped
// directories (.git, bin/, dist/, .nexus/).
func TestWatchSource_IgnoresIrrelevantPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// A dotfile in the root + a Go file in a skipped dir — neither
	// should trigger a rebuild.
	if err := os.WriteFile(filepath.Join(dir, ".swp"), []byte("buffer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "noise.go"), []byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		t.Fatal("watcher should not fire for hidden / skipped paths")
	case <-time.After(400 * time.Millisecond):
		// expected — debounce window passed without a signal
	}
}

// TestWatchSource_FiresOnEmbedTarget verifies the watcher rebuilds
// when a file inside an //go:embed-referenced directory changes — even
// when that directory is named "dist" (normally skipped). Without
// this, `vite build` rewriting web/dist/ wouldn't reach the running
// binary and the embedded SPA would stay stale across the user's
// frontend rebuild.
func TestWatchSource_FiresOnEmbedTarget(t *testing.T) {
	dir := t.TempDir()
	// User code: //go:embed all:web/dist
	src := filepath.Join(dir, "main.go")
	srcBody := "package main\n\nimport \"embed\"\n\n//go:embed all:web/dist\nvar webFS embed.FS\n\nfunc main() { _ = webFS }\n"
	if err := os.WriteFile(src, []byte(srcBody), 0o644); err != nil {
		t.Fatal(err)
	}
	dist := filepath.Join(dir, "web", "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dist, "index.html")
	if err := os.WriteFile(indexPath, []byte("<html>v1</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Simulate `vite build` rewriting the bundle.
	if err := os.WriteFile(indexPath, []byte("<html>v2</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		// expected: embed-rooted .html change fires a rebuild
	case <-time.After(2 * time.Second):
		t.Fatal("expected restart signal after embed-target write")
	}
}

// TestWatchSource_IgnoreSuppressesViteOutput verifies that writes
// under the caller's ignore root (the frontend project dir) DON'T
// fire a rebuild even when those paths sit inside an //go:embed
// target. Without this, vite re-emitting web/dist/* would loop us
// through the embed-root override on every Go restart.
func TestWatchSource_IgnoreSuppressesViteOutput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	mainBody := "package main\n\nimport \"embed\"\n\n//go:embed web/dist/*\nvar distFS embed.FS\n\nfunc main() { _ = distFS }\n"
	if err := os.WriteFile(src, []byte(mainBody), 0o644); err != nil {
		t.Fatal(err)
	}
	dist := filepath.Join(dir, "web", "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dist, "index.html")
	if err := os.WriteFile(indexPath, []byte("<html>v1</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 4)
	ignore := []string{filepath.Join(dir, "web")}
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, ignore); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// Simulate vite re-emitting dist/index.html.
	if err := os.WriteFile(indexPath, []byte("<html>v2</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		t.Fatal("vite write under ignore root should not fire — would loop")
	case <-time.After(400 * time.Millisecond):
		// expected
	}
}

// TestWatchSource_IgnoreFiresOnGoUnderFrontend verifies that a .go
// file kept inside the frontend dir (e.g. web/embed.go for the SPA's
// `package web` helper) still bounces the Go process on save. The
// ignore is meant to suppress vite's artifact writes, not silence
// every file in the tree — a Go source change is always relevant.
func TestWatchSource_IgnoreFiresOnGoUnderFrontend(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(dir, "web")
	if err := os.MkdirAll(web, 0o755); err != nil {
		t.Fatal(err)
	}
	embedGo := filepath.Join(web, "embed.go")
	if err := os.WriteFile(embedGo, []byte("package web\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 4)
	ignore := []string{web}
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, ignore); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	if err := os.WriteFile(embedGo, []byte("package web\n// edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("expected restart on Go file save inside ignored frontend dir")
	}
}

// TestWatchSource_DebouncesBurst verifies that 5 rapid writes
// produce ONE rebuild signal, not 5. Editors that save with
// atomic-rename or write multiple files for one Cmd-S would
// otherwise trigger a rebuild storm.
func TestWatchSource_DebouncesBurst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 4)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	// 5 writes 20ms apart — well under the 200ms debounce window.
	for i := 0; i < 5; i++ {
		_ = os.WriteFile(src, []byte("package main\n"), 0o644)
		time.Sleep(20 * time.Millisecond)
	}

	// One signal should land within the debounce + buffer.
	select {
	case <-out:
	case <-time.After(1 * time.Second):
		t.Fatal("expected at least one debounced signal")
	}

	// No additional signals should follow within a short window
	// after the debounce timer fires.
	select {
	case <-out:
		t.Error("burst should coalesce to one signal; got a second")
	case <-time.After(300 * time.Millisecond):
		// expected
	}
}

// TestWatchSource_FiresOnNewPackageDir covers adding a package
// mid-session: `mkdir pkg && write pkg/x.go` is one editor action, so the
// file can land before the new directory joins the watch set. The tree
// scan on directory-create is what keeps that save from being swallowed —
// and the dir must end up watched, or every later edit inside it would be
// swallowed too.
func TestWatchSource_FiresOnNewPackageDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	pkg := filepath.Join(dir, "billing")
	if err := os.Mkdir(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(pkg, "billing.go")
	if err := os.WriteFile(src, []byte("package billing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no rebuild signal for a newly created package dir")
	}

	// And the new dir is now watched: a later edit inside it must fire too.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(src, []byte("package billing\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("new package dir was not added to the watch set")
	}
}

// A created directory that holds nothing the build cares about must not
// bounce the app — mkdir alone is not a code change.
func TestWatchSource_IgnoresNewDirWithoutBuildInputs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, nil); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	docs := filepath.Join(dir, "docs")
	if err := os.Mkdir(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(docs, "notes.md"), []byte("# hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-out:
		t.Fatal("rebuilt for a directory with no build inputs")
	case <-time.After(600 * time.Millisecond):
	}
}

// startWatch boots a watcher on dir and returns its signal channel.
func startWatch(t *testing.T, dir string, ignore []string) chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	out := make(chan struct{}, 1)
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}, ignore); err != nil {
		t.Fatalf("watchSource: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	return out
}

func expectNoSignal(t *testing.T, out <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-out:
		t.Fatalf("rebuilt for %s", what)
	case <-time.After(600 * time.Millisecond):
	}
}

// `go build` never compiles test files, so editing one can't change the
// binary — restarting the app would only cost the developer its state.
func TestWatchSource_IgnoresTestFiles(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "store.go")
	test := filepath.Join(dir, "store_test.go")
	for _, f := range []string{src, test} {
		if err := os.WriteFile(f, []byte("package app\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := startWatch(t, dir, nil)

	if err := os.WriteFile(test, []byte("package app\n// edited test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectNoSignal(t, out, "a _test.go edit")

	// Sanity: the same watcher still fires for real source next to it.
	if err := os.WriteFile(src, []byte("package app\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal for a non-test .go edit")
	}
}

func TestWatchSource_IgnoresTestdata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtures := filepath.Join(dir, "testdata")
	if err := os.Mkdir(fixtures, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixtures, "golden.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := startWatch(t, dir, nil)

	if err := os.WriteFile(filepath.Join(fixtures, "golden.go"), []byte("package x\n// changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectNoSignal(t, out, "a testdata fixture")
}

// A nested go.mod is outside the root module's ./..., so its files never
// reach the binary — unless the root module replaces into it.
func TestWatchSource_NestedModules(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.test/app\n\ngo 1.22\n\nreplace example.test/lib => ./libs/lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkmod := func(rel string) string {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "go.mod"), []byte("module example.test/"+filepath.Base(rel)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(p, "x.go")
		if err := os.WriteFile(src, []byte("package lib\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return src
	}
	separate := mkmod("tools/gen")
	replaced := mkmod("libs/lib")

	out := startWatch(t, dir, nil)

	if err := os.WriteFile(separate, []byte("package lib\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectNoSignal(t, out, "an edit in an unreferenced nested module")

	if err := os.WriteFile(replaced, []byte("package lib\n// edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal for a nested module the root module replaces into")
	}
}

// A project's own .nexusignore keeps listed paths out of the dev loop:
// generated trees, scratch dirs, a vendored sibling service.
func TestWatchSource_HonorsNexusIgnore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".nexusignore"),
		[]byte("# project rules\ngenerated/\nlegacy/*.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"generated", "legacy"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "x.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := startWatch(t, dir, nil)

	if err := os.WriteFile(filepath.Join(dir, "generated", "x.go"), []byte("package x\n// gen\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectNoSignal(t, out, "a write inside an ignored directory")

	if err := os.WriteFile(filepath.Join(dir, "legacy", "x.go"), []byte("package x\n// legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expectNoSignal(t, out, "a write matching an ignored glob")

	// Everything else still rebuilds.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n// edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("no signal for a file outside .nexusignore")
	}
}
