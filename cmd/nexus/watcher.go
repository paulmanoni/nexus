package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchSource starts a recursive file watcher rooted at root and
// emits debounced rebuild signals on out whenever a Go source file
// (or go.mod / go.sum / nexus.deploy.yaml) under root changes.
//
// Debounce: 200ms after the last burst event. Editors commonly write
// several files for one Cmd-S (atomic-rename via .tmp, dotfile writes
// for buffer state, etc.); coalescing avoids triggering N rebuilds
// per save.
//
// Skipped paths: .git, .nexus, bin, dist, node_modules, vendor,
// hidden directories. .nexus/build is the codegen output; rebuilding
// on its writes would loop forever.
//
// Cancellation: closing ctx stops the watcher goroutine and closes
// the underlying fsnotify watcher. out is left open — consumers
// that select on ctx.Done() shut down naturally.
func watchSource(ctx context.Context, root string, out chan<- struct{}, stderr io.Writer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := addWatchDirs(w, root); err != nil {
		w.Close()
		return fmt.Errorf("watch %s: %w", root, err)
	}
	go func() {
		defer w.Close()
		var debounce *time.Timer
		fire := func() {
			select {
			case out <- struct{}{}:
			default:
				// out already has a pending signal — drop this one,
				// the consumer will pick up the next change anyway.
			}
		}
		for {
			select {
			case <-ctx.Done():
				if debounce != nil {
					debounce.Stop()
				}
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !relevantEvent(ev) {
					continue
				}
				// New directory created? Add it to the watch list so
				// edits inside fire restarts. Common when a user adds
				// a new module package mid-session.
				if ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = addWatchDirs(w, ev.Name)
					}
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(200*time.Millisecond, fire)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(stderr, "watcher: %v\n", err)
			}
		}
	}()
	return nil
}

// addWatchDirs walks root recursively and adds every directory the
// watcher should track. Hidden + skip-listed dirs short-circuit via
// SkipDir so we don't descend into them. fsnotify watches files via
// their parent dir, so adding the dir is sufficient to catch every
// file write inside it.
func addWatchDirs(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than aborting the whole walk
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && shouldSkipDir(name) {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// shouldSkipDir returns true for directories the watcher should never
// descend into. Hidden dirs (.git, .vscode, .idea) are always skipped;
// the rest is an explicit allowlist of dirs that produce noise without
// signal.
func shouldSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "bin", "dist", "build":
		return true
	}
	return false
}

// relevantEvent reports whether ev should trigger a rebuild. Write,
// Create, Rename, and Remove all matter — Create catches `mv` rename
// targets, Remove + Rename catch the source side of an atomic-rename
// save. Chmod-only events are ignored (don't rebuild on `chmod +x`).
//
// File-name filter: .go source, plus go.mod / go.sum (dep changes
// alter the build) and nexus.deploy.yaml (codegen consumes it).
// Hidden files are skipped — editors write dotfile buffer state
// during normal saves and we don't want to rebuild on those.
func relevantEvent(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") {
		return false
	}
	if strings.HasSuffix(base, ".go") {
		return true
	}
	switch base {
	case "go.mod", "go.sum", "nexus.deploy.yaml":
		return true
	}
	return false
}
