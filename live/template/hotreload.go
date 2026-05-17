package template

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

// WatchHotReload starts an fsnotify watcher on dir and re-registers
// any registered component whose backing .nlt file changes on disk.
// On every successful reload, an Outbound{Type:"reload"} frame fans
// out to every connected session — the client JS already handles
// that type by calling location.reload(), so the browser picks up
// the new template without a manual refresh.
//
// Component name is inferred from the filename stem
// ("templates/Posts.nlt" → "Posts"). Files whose stem doesn't
// match a registered component are silently skipped, so unrelated
// .nlt files in the same dir won't error.
//
// dir is non-recursive — fsnotify watches one directory at a time
// and the typical project layout is one templates/ folder per app.
// Apps with nested template trees can call WatchHotReload once per
// sub-directory.
//
// Returns immediately; the watcher runs in its own goroutine until
// ctx is cancelled. Setup errors (bad dir, fsnotify init failure)
// are returned synchronously so the caller's lifecycle hook sees
// them; runtime watch errors are logged and the loop continues.
//
// Intended for development. In production the templates are baked
// into the binary via embed.FS — the on-disk files don't matter,
// and the watcher just consumes a couple of file descriptors.
// Gate the call behind your own env / build-tag check.
func (e *Engine) WatchHotReload(ctx context.Context, dir string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("template: hot-reload watcher: %w", err)
	}
	if err := w.Add(dir); err != nil {
		_ = w.Close()
		return fmt.Errorf("template: hot-reload watch %s: %w", dir, err)
	}

	go func() {
		defer w.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
					continue
				}
				if !strings.HasSuffix(ev.Name, ".nlt") {
					continue
				}
				name := strings.TrimSuffix(filepath.Base(ev.Name), ".nlt")
				if _, registered := e.lookup(name); !registered {
					// Unknown component — could be a new file or
					// one with a non-matching name. Skip without
					// noise; users hot-add new components by
					// restarting the binary anyway.
					continue
				}
				src, err := os.ReadFile(ev.Name)
				if err != nil {
					log.Printf("template: hot-reload read %s: %v", ev.Name, err)
					continue
				}
				if err := e.Reload(name, src); err != nil {
					// A parse error on the new source is a
					// programmer mistake mid-edit; log it and keep
					// the old template active so the page stays
					// usable until the save settles.
					log.Printf("template: hot-reload %s: %v", name, err)
					continue
				}
				log.Printf("template: hot-reloaded %s", name)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("template: hot-reload watcher: %v", err)
			}
		}
	}()
	return nil
}
