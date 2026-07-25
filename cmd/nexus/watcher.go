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
// (or go.mod / go.sum / nexus.toml) under root changes.
//
// Debounce: 200ms after the last burst event. Editors commonly write
// several files for one Cmd-S (atomic-rename via .tmp, dotfile writes
// for buffer state, etc.); coalescing avoids triggering N rebuilds
// per save.
//
// Skipped paths: .git, .nexus, bin, dist, node_modules, vendor,
// testdata, hidden directories. .nexus/build is the codegen output;
// rebuilding on its writes would loop forever. Nested modules (a subdir
// with its own go.mod) are skipped too unless the root module replaces
// into them — otherwise they're outside this build entirely.
//
// Scoped to the app's build inputs: _test.go files are watched by
// nobody here, since `go build` never compiles them. Editing a test
// while the app runs used to cost a full rebuild + restart.
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
func watchSource(ctx context.Context, root string, out chan<- struct{}, stderr io.Writer, ignore []string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	scope := newWatchScope(root, ignore)
	if err := scope.addDirs(w, root); err != nil {
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
				rebuild := scope.relevant(ev)
				// New directory created? Add it to the watch list so
				// edits inside fire restarts. Common when a user adds
				// a new module package mid-session, or when a frontend
				// build creates fresh subdirs under an embed root.
				//
				// This runs BEFORE the relevance verdict is final: a
				// directory name is never a build input, so filtering
				// first would leave the new tree unwatched forever. The
				// files inside can also land before the watch does
				// (mkdir + write is one editor action), so a new tree
				// that already holds build inputs counts as a change.
				if ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = scope.addDirs(w, ev.Name)
						if !rebuild && scope.treeHasBuildInput(ev.Name) {
							rebuild = true
						}
					}
				}
				if !rebuild {
					continue
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

// watchScope holds the per-session rules that decide which directories
// the watcher tracks and which events count as a change: the //go:embed
// roots, the frontend-owned ignore tree, and the nested modules this
// build doesn't include. Computed once at startup so the per-event
// checks stay cheap.
type watchScope struct {
	embedRoots map[string]bool
	ignore     []string // absolute dirs the frontend toolchain owns
	replaces   []string // absolute local `replace =>` targets of the root module
	userIgnore *ignoreMatcher
}

func newWatchScope(root string, ignore []string) *watchScope {
	s := &watchScope{
		embedRoots: scanEmbedTargets(root),
		userIgnore: loadNexusIgnore(root),
	}
	for _, p := range ignore {
		if abs, err := filepath.Abs(p); err == nil && abs != "" {
			s.ignore = append(s.ignore, abs)
		}
	}
	s.replaces = localReplaceTargets(root)
	return s
}

// separateModule reports whether dir is its own Go module that this build
// can't see. A nested go.mod is outside the root module's `./...`, so its
// files never reach the binary — unless the root module replaces into it,
// which is how monorepos wire a local library and very much is a build
// input.
func (s *watchScope) separateModule(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	return !pathUnder(dir, s.replaces)
}

// addDirs walks root recursively and adds every directory the
// watcher should track. Hidden + skip-listed dirs short-circuit via
// SkipDir so we don't descend into them. fsnotify watches files via
// their parent dir, so adding the dir is sufficient to catch every
// file write inside it.
//
// Embed override: a normally-skipped dir gets traversed anyway when
// it is, or contains, an //go:embed target — that's how `web/dist`
// changes reach the rebuild signal even though "dist" is in the
// skip-list.
//
// Ignore handling: the caller-supplied ignore list flags directories
// the frontend toolchain owns (e.g. ./web). We still descend into
// them so a Go source file kept alongside the SPA (a `package web`
// embed helper) bounces the binary on save, but we ONLY register
// dirs that contain Go source — not artifact subdirs like web/dist
// or web/sdk that vite would write to and loop us. shouldSkipDir
// stays in force inside the ignore tree (no embed override) since
// the frontend bytes don't need to reach the binary; ServeFrontend
// reads them off disk in dev.
func (s *watchScope) addDirs(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than aborting the whole walk
		}
		if !d.IsDir() {
			return nil
		}
		underIgnore := pathUnder(path, s.ignore)
		name := d.Name()
		if path != root && shouldSkipDir(name) {
			// Inside the ignore tree the embed-override is suppressed
			// (vite owns those bytes); outside, we still walk into
			// embed targets so a non-dev build still rebuilds when
			// the embedded SPA changes.
			if underIgnore || !embedDirOrAncestor(path, s.embedRoots) {
				return filepath.SkipDir
			}
		}
		if path != root && s.separateModule(path) {
			return filepath.SkipDir
		}
		// A .nexusignore'd directory is pruned outright: nothing inside it
		// is watched, so its saves can't reach the rebuild signal.
		if path != root && s.userIgnore.match(path, true) {
			return filepath.SkipDir
		}
		if underIgnore && !dirHasGoSource(path) {
			// Frontend dir without Go source: don't watch. We still
			// recurse so a nested package (web/internal/foo.go) is
			// reachable.
			return nil
		}
		return w.Add(path)
	})
}

// treeHasBuildInput reports whether a newly created directory already
// contains something that would have triggered a rebuild had we been
// watching when it was written. Same verdict as relevant(), applied per
// file, so embed roots, the ignore tree and nested modules behave
// identically. Only ever called on a directory Create.
func (s *watchScope) treeHasBuildInput(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && (shouldSkipDir(d.Name()) || s.separateModule(path) || s.userIgnore.match(path, true)) {
				return filepath.SkipDir
			}
			return nil
		}
		if s.relevant(fsnotify.Event{Name: path, Op: fsnotify.Create}) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// dirHasGoSource reports whether dir directly contains a .go file
// (test files included — they're still build inputs). Used to decide
// whether a directory inside the caller's ignore list still deserves
// a watch.
func dirHasGoSource(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
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
	case "node_modules", "vendor", "bin", "dist", "build", "testdata":
		// testdata is invisible to the go tool by definition — fixtures
		// change constantly while working on tests and never affect the
		// binary.
		return true
	}
	return false
}

// relevant reports whether ev should trigger a rebuild. Write,
// Create, Rename, and Remove all matter — Create catches `mv` rename
// targets, Remove + Rename catch the source side of an atomic-rename
// save. Chmod-only events are ignored (don't rebuild on `chmod +x`).
//
// File-name filter: .go source, plus go.mod / go.sum (dep changes
// alter the build) and nexus.toml (codegen consumes it). Any
// non-hidden file under an //go:embed root also counts — the binary
// has to recompile to repackage the new bundle bytes.
//
// Ignore handling: events under the caller's ignore tree fire only
// for Go-build-relevant files. That keeps web/embed.go saves bouncing
// the binary while suppressing vite's web/dist + web/sdk writes that
// would otherwise loop us through the embed-root override.
//
// Hidden files are skipped — editors write dotfile buffer state
// during normal saves and we don't want to rebuild on those.
func (s *watchScope) relevant(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") {
		return false
	}
	// The project's own .nexusignore wins over every rule below: a path it
	// lists is out of the dev loop, embed roots included.
	if s.userIgnore.match(ev.Name, false) {
		return false
	}
	if pathUnder(ev.Name, s.ignore) {
		return isGoBuildFile(base)
	}
	if underEmbedRoot(ev.Name, s.embedRoots) {
		return true
	}
	return isGoBuildFile(base)
}

// isGoBuildFile reports whether base names a file whose change
// invalidates the Go build: any .go source, go.mod / go.sum (deps),
// or nexus.toml (codegen consumes it).
//
// _test.go is excluded: `go build` never compiles test files, so the
// binary can't change and the app has no reason to restart. Editing a
// test while the server runs is a normal thing to do.
func isGoBuildFile(base string) bool {
	if strings.HasSuffix(base, ".go") {
		return !strings.HasSuffix(base, "_test.go")
	}
	switch base {
	case "go.mod", "go.sum", "nexus.toml":
		return true
	}
	return false
}

// localReplaceTargets returns the absolute directories the module
// governing dir replaces into (`replace x => ./libs/x`). Those trees ARE
// build inputs despite carrying their own go.mod, so the watcher must
// keep watching them.
//
// Parsed by hand rather than with modfile: the only thing we need is the
// right-hand side of a `=>` when it names a filesystem path, and a
// false positive here just means one extra directory stays watched.
func localReplaceTargets(dir string) []string {
	modDir := findModuleRoot(dir)
	if modDir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(modDir, "go.mod"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		i := strings.Index(line, "=>")
		if i < 0 {
			continue
		}
		target := strings.TrimSpace(line[i+2:])
		if target == "" || !(strings.HasPrefix(target, ".") || filepath.IsAbs(target)) {
			continue // a module path + version, not a local directory
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(modDir, target)
		}
		if abs, err := filepath.Abs(target); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// pathUnder reports whether path is the same as, or nested under,
// any prefix in roots. Each root is treated as a directory boundary
// (matching `<root>` exactly or `<root>/...`); a partial-name match
// like "/web2" against "/web" doesn't count as a hit.
func pathUnder(path string, roots []string) bool {
	if len(roots) == 0 {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, r := range roots {
		if abs == r {
			return true
		}
		if strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return true
		}
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
	// os.OpenRoot pins the walk to root; subsequent rt.Open calls
	// refuse to traverse symlinks that point outside the root,
	// closing the TOCTOU window between WalkDir resolving a path
	// and os.Open following it.
	rt, err := os.OpenRoot(root)
	if err != nil {
		return out
	}
	defer rt.Close()
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		f, err := rt.Open(rel)
		if err != nil {
			return nil
		}
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
		f.Close()
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
