package main

import (
	"bufio"
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
// Embed-aware: scans .go files for //go:embed directives once at
// startup and tracks those paths even when they fall under normally
// skipped names ("dist", "build"). Any non-hidden file change under
// an embed root fires a rebuild — vite/webpack rewriting web/dist/
// must reach the binary, otherwise the embedded SPA stays stale
// across `npm run build`.
//
// Cancellation: closing ctx stops the watcher goroutine and closes
// the underlying fsnotify watcher. out is left open — consumers
// that select on ctx.Done() shut down naturally.
func watchSource(ctx context.Context, root string, out chan<- struct{}, stderr io.Writer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	embedRoots := scanEmbedTargets(root)
	if err := addWatchDirs(w, root, embedRoots); err != nil {
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
				if !relevantEvent(ev, embedRoots) {
					continue
				}
				// New directory created? Add it to the watch list so
				// edits inside fire restarts. Common when a user adds
				// a new module package mid-session, or when a frontend
				// build creates fresh subdirs under an embed root.
				if ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = addWatchDirs(w, ev.Name, embedRoots)
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
//
// Embed override: a normally-skipped dir gets traversed anyway when
// it is, or contains, an //go:embed target — that's how `web/dist`
// changes reach the rebuild signal even though "dist" is in the
// skip-list.
func addWatchDirs(w *fsnotify.Watcher, root string, embedRoots map[string]bool) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than aborting the whole walk
		}
		if !d.IsDir() {
			return nil
		}
		name := d.Name()
		if path != root && shouldSkipDir(name) && !embedDirOrAncestor(path, embedRoots) {
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
// alter the build) and nexus.deploy.yaml (codegen consumes it). Any
// non-hidden file under an //go:embed root also counts — the binary
// has to recompile to repackage the new bundle bytes.
//
// Hidden files are skipped — editors write dotfile buffer state
// during normal saves and we don't want to rebuild on those.
func relevantEvent(ev fsnotify.Event, embedRoots map[string]bool) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") {
		return false
	}
	if underEmbedRoot(ev.Name, embedRoots) {
		return true
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

// scanEmbedTargets walks root looking for //go:embed directives in
// .go files and returns the set of absolute paths they reference.
// Patterns are resolved relative to the .go file's directory; "all:"
// prefixes and globs are honored. Best-effort — malformed or missing
// targets are silently dropped, the watcher just won't pick them up.
//
// The scan reuses shouldSkipDir to avoid descending into node_modules
// / vendor / hidden dirs while looking for source. Embed targets
// themselves can still live under skip-listed names (web/dist) —
// they're discovered via the .go directives that point at them, not
// by walking into dist/.
func scanEmbedTargets(root string) map[string]bool {
	out := map[string]bool{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		dir := filepath.Dir(path)
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "//go:embed") {
				continue
			}
			for _, pat := range strings.Fields(line)[1:] {
				pat = strings.TrimPrefix(pat, "all:")
				full := pat
				if !filepath.IsAbs(full) {
					full = filepath.Join(dir, pat)
				}
				addEmbedMatches(out, full)
			}
		}
		return nil
	})
	return out
}

// addEmbedMatches resolves an //go:embed pattern (already made
// absolute) into concrete on-disk paths and records them. Falls back
// to a direct stat when filepath.Glob returns no matches — patterns
// without metacharacters are valid embed targets but Glob returns
// nil for them.
func addEmbedMatches(out map[string]bool, pattern string) {
	matches, err := filepath.Glob(pattern)
	if err == nil && len(matches) > 0 {
		for _, m := range matches {
			if abs, err := filepath.Abs(m); err == nil {
				out[abs] = true
			}
		}
		return
	}
	if _, err := os.Stat(pattern); err == nil {
		if abs, err := filepath.Abs(pattern); err == nil {
			out[abs] = true
		}
	}
}

// embedDirOrAncestor reports whether path is an embed root or an
// ancestor directory of one. Used by addWatchDirs to decide whether
// to descend into a normally-skipped name like "dist" or "build".
func embedDirOrAncestor(path string, embedRoots map[string]bool) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for root := range embedRoots {
		if root == abs {
			return true
		}
		if strings.HasPrefix(root, abs+string(filepath.Separator)) {
			return true
		}
		if strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// underEmbedRoot reports whether path lives at or below any embed
// root. Used by relevantEvent so non-Go files under web/dist still
// trigger a rebuild when the bundle gets regenerated.
func underEmbedRoot(path string, embedRoots map[string]bool) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for root := range embedRoots {
		if abs == root {
			return true
		}
		if strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}