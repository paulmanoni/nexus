package main

import (
	"bufio"
	"bytes"
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

// distDebounce coalesces a burst of saves into one rebuild.
const distDebounce = 600 * time.Millisecond

// watchDistBuild keeps the frontend's build output continuously in sync
// with its source while `nexus dev` runs. The Vite dev server serves the
// live frontend from memory and never writes dist, so without this the
// embedded production bundle (//go:embed all:web/dist) stays frozen at
// whatever the last `nexus build` produced — a `go build` taken mid-session
// ships stale assets. With --dist on, every debounced source change runs
// the project's own `vite build` (and its SSR build, when src/ssr.ts
// exists — the client build's emptyOutDir would otherwise delete it).
//
// The dev server keeps working throughout: nexus-vite-plugin stashes the
// live hot file before the build empties the outDir and writes it back
// after, so the app never loses its dev server. The first build waits for
// vite (when one runs) to have written that file — a hot file that
// appears mid-build, after the stash, would be emptied away.
//
// Builds never overlap: one worker runs them, and a change during a build
// queues exactly one more. No rebuild loop: dist/ and node_modules are not
// watched, and the Go-source watcher ignores the whole frontend dir.
//
// The returned stop ends the watcher and returns once nothing is left
// running — a build in flight is stopped like the dev server (SIGTERM,
// then SIGKILL); defer it. Cancelling ctx stops everything too.
func watchDistBuild(ctx context.Context, webDir, tomlPath string, vite *devVite, userIgnore *ignoreMatcher, stdout, stderr io.Writer) (stop func(), err error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := addDistWatchDirs(w, webDir, userIgnore); err != nil {
		w.Close()
		return nil, err
	}
	env, err := frontendEnv(tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s●%s [dist] [env] not passed to Vite: %v\n", ansiYellow, ansiReset, err)
		env = os.Environ()
	}
	ctx, cancel := context.WithCancel(ctx)

	logf := func(format string, args ...any) {
		fmt.Fprintf(stdout, "%s[dist]%s %s\n", ansiCyan, ansiReset, fmt.Sprintf(format, args...))
	}
	build := func() {
		start := time.Now()
		steps := [][]string{{"build"}}
		if fileExists(filepath.Join(webDir, "src", "ssr.ts")) {
			steps = append(steps, []string{"build", "--ssr", "src/ssr.ts", "--outDir", "dist/ssr"})
		}
		for _, args := range steps {
			out, err := runViteOnce(ctx, webDir, env, args...)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				fmt.Fprintf(stderr, "%s●%s [dist] vite %s failed: %v\n", ansiYellow, ansiReset, strings.Join(args, " "), err)
				sc := bufio.NewScanner(bytes.NewReader(out))
				for sc.Scan() {
					fmt.Fprintf(stderr, "%s[dist]%s %s\n", ansiCyan, ansiReset, sc.Text())
				}
				return
			}
		}
		logf("rebuilt in %s", time.Since(start).Round(time.Millisecond))
	}

	trigger := make(chan struct{}, 1)
	kick := func() {
		select {
		case trigger <- struct{}{}:
		default: // one already queued; it will see this change too
		}
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if vite != nil {
			select {
			case <-ctx.Done():
				return
			case <-vite.settledCh():
			}
		}
		// Build once up front so dist matches the current source the
		// moment dev starts, not only after the first edit.
		logf("watching %s · dist mirrors the frontend on every change", webDir)
		build()
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				build()
			}
		}
	}()

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		defer w.Close()
		var debounce *time.Timer
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
				if !distRelevant(ev) || userIgnore.match(ev.Name, false) {
					continue
				}
				// A new source subdir appeared mid-session (new feature
				// folder, etc.) — start watching it so edits inside fire
				// rebuilds. Skip the output/dep dirs so we never watch dist.
				if ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() && !distSkipDir(filepath.Base(ev.Name)) {
						_ = addDistWatchDirs(w, ev.Name, userIgnore)
					}
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(distDebounce, kick)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(stderr, "[dist] watcher: %v\n", err)
			}
		}
	}()

	return func() {
		cancel()
		<-watcherDone
		<-workerDone
	}, nil
}

// runViteOnce runs the project's Vite to completion with args and returns
// its combined output. When ctx ends first, Vite is stopped the way the dev
// server is (SIGTERM to its group, then SIGKILL) and waited for.
func runViteOnce(ctx context.Context, webDir string, env []string, args ...string) ([]byte, error) {
	cmd, err := viteCmd(context.Background(), webDir, env, args...)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	var werr error
	go func() {
		werr = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		stopProcessGroup(cmd.Process.Pid, done, viteKillGrace, nil)
	}
	return out.Bytes(), werr
}

// addDistWatchDirs registers root and its subdirs with the watcher,
// skipping the build output (dist), installed deps (node_modules),
// hidden/cache dirs so the build's own writes never retrigger it, and
// whatever the project's .nexusignore lists.
func addDistWatchDirs(w *fsnotify.Watcher, root string, userIgnore *ignoreMatcher) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable subtrees rather than abort the walk
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (distSkipDir(d.Name()) || userIgnore.match(path, true)) {
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// distSkipDir lists the directories the dist watcher never descends into: the
// build output itself (dist — watching it would loop), installed deps, and
// hidden/cache dirs (.git, .vite, …).
func distSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "dist", "node_modules":
		return true
	}
	return false
}

// distRelevant filters watcher events down to real source writes: content ops
// only (chmod ignored) on non-hidden files. The dist/ tree is already excluded
// at the directory level, so any event that reaches here is genuine source.
func distRelevant(ev fsnotify.Event) bool {
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	if strings.HasPrefix(filepath.Base(ev.Name), ".") {
		return false
	}
	return true
}
