package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestWaitForFrontendEntries_ImmediateWhenAlreadyPresent: steady-
// state path — entries already exist, helper returns instantly
// without any fsnotify setup.
func TestWaitForFrontendEntries_ImmediateWhenAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "islands.src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "main.ts"), []byte("// entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, got, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "main.ts") {
		t.Errorf("entries: %v", got)
	}
}

// TestWaitForFrontendEntries_AppearsLater: src dir starts empty,
// a file lands ~100ms later, helper picks it up and returns it.
func TestWaitForFrontendEntries_AppearsLater(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "islands.src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type result struct {
		entries []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
		done <- result{entries, err}
	}()

	// Give the goroutine a moment to install its fsnotify watcher
	// before we drop the file in.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(srcDir, "main.ts"), []byte("// entry"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("err: %v", r.err)
		}
		if len(r.entries) != 1 {
			t.Errorf("entries: %v", r.entries)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("helper did not return within 2s of file creation")
	}
}

// TestWaitForFrontendEntries_DirCreatedLater: islands.src doesn't
// exist when the helper starts. The dir is created + a file
// dropped in ~150ms later. Helper transitions through both
// states and returns the entry.
func TestWaitForFrontendEntries_DirCreatedLater(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "islands.src")
	// NOTE: srcDir does NOT exist yet.

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	type result struct {
		entries []string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		_, entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
		done <- result{entries, err}
	}()

	time.Sleep(150 * time.Millisecond)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(srcDir, "main.ts"), []byte("// entry"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("err: %v", r.err)
		}
		if len(r.entries) != 1 {
			t.Errorf("entries: %v", r.entries)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not return within 3s")
	}
}

// TestWaitForFrontendEntries_AutoDescendsIntoSrcSubdir mirrors
// the real-world case where the operator drops a Vite-style
// project under islands.src/: bootstrap lives at islands.src/src/
// main.ts, not islands.src/main.ts. Auto-descent must find it
// and return the nested dir as the actual srcDir.
func TestWaitForFrontendEntries_AutoDescendsIntoSrcSubdir(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "islands.src")
	srcSubdir := filepath.Join(srcDir, "src")
	if err := os.MkdirAll(srcSubdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcSubdir, "main.ts"), []byte("// entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Top-level decoy files that should NOT be treated as entries.
	if err := os.WriteFile(filepath.Join(srcDir, "index.html"), []byte("<html/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	actualDir, entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if actualDir != srcSubdir {
		t.Errorf("actualDir: got %q, want %q (should auto-descend into src/)", actualDir, srcSubdir)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0], "main.ts") {
		t.Errorf("entries: %v", entries)
	}
}

// TestFindFrontendEntries_TopLevelWinsOverNested verifies the
// search order: when top-level HAS entries, the auto-descent
// MUST NOT happen even if src/ also has entries. Otherwise an
// existing islands-convention project would silently shift to
// scanning src/ on a stray file.
func TestFindFrontendEntries_TopLevelWinsOverNested(t *testing.T) {
	dir := t.TempDir()
	// Both top-level and src/ have entries.
	if err := os.WriteFile(filepath.Join(dir, "main.ts"), []byte("// top"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "main.ts"), []byte("// nested"), 0o644); err != nil {
		t.Fatal(err)
	}

	actualDir, entries, err := findFrontendEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if actualDir != dir {
		t.Errorf("top-level should win — got %q, want %q", actualDir, dir)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0], filepath.Join(dir, "main.ts")) {
		t.Errorf("entries: %v", entries)
	}
}

// TestFindFrontendEntries_AppFallback covers the `app/` fallback
// — nuxt-style layouts ship the bootstrap there. Order is src
// first, app second, client third; only the first hit wins.
func TestFindFrontendEntries_AppFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "main.ts"), []byte("// nuxt"), 0o644); err != nil {
		t.Fatal(err)
	}

	actualDir, entries, err := findFrontendEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(actualDir) != "app" {
		t.Errorf("expected app/ fallback, got %q", actualDir)
	}
	if len(entries) != 1 {
		t.Errorf("entries: %v", entries)
	}
}

// TestDescribeEmptyState classifies the three "why is
// collectFrontendEntries empty" cases so the wait loop can pick
// the right log message + fix hint.
func TestDescribeEmptyState(t *testing.T) {
	t.Run("no files", func(t *testing.T) {
		dir := t.TempDir()
		if got := describeEmptyState(dir); got != emptyStateNoFiles {
			t.Errorf("empty dir: got %d, want emptyStateNoFiles", got)
		}
	})

	t.Run("only .vue", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "App.vue"), []byte("<template/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := describeEmptyState(dir); got != emptyStateOnlyVue {
			t.Errorf(".vue-only: got %d, want emptyStateOnlyVue", got)
		}
	})

	t.Run("subdir only", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "components"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got := describeEmptyState(dir); got != emptyStateNoTopLevel {
			t.Errorf("subdir only: got %d, want emptyStateNoTopLevel", got)
		}
	})

	t.Run("vue + subdir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "App.vue"), []byte("<template/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "components"), 0o755); err != nil {
			t.Fatal(err)
		}
		// Subdir counts as "other" so the message should say "no
		// top-level entry" rather than the .vue-specific bootstrap
		// hint — the operator might be using something other than
		// Vue.
		if got := describeEmptyState(dir); got != emptyStateNoTopLevel {
			t.Errorf("vue + subdir: got %d, want emptyStateNoTopLevel", got)
		}
	})

	t.Run("ignore hidden files", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte{}, 0o644); err != nil {
			t.Fatal(err)
		}
		if got := describeEmptyState(dir); got != emptyStateNoFiles {
			t.Errorf("hidden-only: got %d, want emptyStateNoFiles (hidden files don't count)", got)
		}
	})
}

// TestWaitForFrontendEntries_CtxCancellationReturnsNilNil: a
// cancel during the wait must return (nil, nil) so the caller
// exits quietly. No "ctx cancelled" error noise in the log
// when the operator hits Ctrl-C.
func TestWaitForFrontendEntries_CtxCancellationReturnsNilNil(t *testing.T) {
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "islands.src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		entries []string
		err     error
	}, 1)
	go func() {
		_, entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
		done <- struct {
			entries []string
			err     error
		}{entries, err}
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err != nil {
			t.Errorf("ctx cancel should not error, got %v", r.err)
		}
		if r.entries != nil {
			t.Errorf("ctx cancel should return nil entries, got %v", r.entries)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("helper did not return after ctx cancel")
	}
}
