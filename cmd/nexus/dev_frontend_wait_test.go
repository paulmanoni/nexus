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

	got, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
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
		entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
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
		entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
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
		entries, err := waitForFrontendEntries(ctx, srcDir, "islands.src", &bytes.Buffer{})
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
