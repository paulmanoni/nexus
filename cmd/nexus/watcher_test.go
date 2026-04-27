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
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}); err != nil {
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
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}); err != nil {
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
	if err := watchSource(ctx, dir, out, &bytes.Buffer{}); err != nil {
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
