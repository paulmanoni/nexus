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

	"github.com/paulmanoni/viteless"
)

// watchDistBuild keeps <frontendDir>/dist continuously in sync with the
// frontend source while `nexus dev` runs. The HMR dev server (viteless.Dev)
// serves the live frontend from memory and never writes dist, so without this
// the embedded production bundle (//go:embed web/dist) stays frozen at whatever
// the last `nexus build` produced — a `go build` taken mid-session ships stale
// assets. With --dist on, every debounced source change fires a background
// viteless.Build into web/dist so the embed always matches the current
// frontend, no manual `nexus build` needed.
//
// Cost: this runs a full production bundle alongside the HMR server, which is
// why it's opt-in. esbuild plus the cached dep store keep an incremental,
// app-only rebuild fast — deps are fetched once and then read from CacheRoot,
// so the recurring work is just the app's own modules. Builds are debounced
// and coalesced so a burst of saves yields one rebuild.
//
// No rebuild loop: the dist/ output dir is excluded from the watch (the build
// writes only there), and the Go-source watcher already suppresses restarts on
// web/dist writes because the frontend dir is in its ignore tree — so a dist
// rebuild neither retriggers itself nor bounces the Go process.
func watchDistBuild(ctx context.Context, frontendDir string, env map[string]string, userIgnore *ignoreMatcher, stdout, stderr io.Writer) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	if err := addDistWatchDirs(w, frontendDir, userIgnore); err != nil {
		w.Close()
		return err
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(stdout, "%s[dist]%s %s\n", ansiCyan, ansiReset, fmt.Sprintf(format, args...))
	}

	build := func() {
		start := time.Now()
		res, err := viteless.Build(viteless.BuildConfig{
			Root: frontendDir,
			Env:  env,
			// Swallow the per-file viteless chatter — it's noise on every
			// rebuild; we print a single summary line below instead.
			Logf: func(string, ...any) {},
		})
		if err != nil {
			fmt.Fprintf(stderr, "%s●%s [dist] build failed: %v\n", ansiYellow, ansiReset, err)
			return
		}
		if len(res.Errors) > 0 {
			for _, e := range res.Errors {
				fmt.Fprintf(stderr, "%s●%s [dist] %s\n", ansiYellow, ansiReset, e)
			}
			return
		}
		logf("rebuilt → %s (%d files · %s)", res.OutDir, len(res.OutputFiles), time.Since(start).Round(time.Millisecond))
	}

	// Build once up front so dist matches the current source the moment
	// dev starts, not only after the first edit.
	logf("watching %s · web/dist mirrors the frontend on every change", frontendDir)
	build()

	go func() {
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
				debounce = time.AfterFunc(600*time.Millisecond, build)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				fmt.Fprintf(stderr, "[dist] watcher: %v\n", err)
			}
		}
	}()
	return nil
}

// addDistWatchDirs registers frontendDir and its subdirs with the watcher,
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
