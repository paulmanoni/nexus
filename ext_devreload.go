package nexus

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gin-gonic/gin"
)

// Dev-only live reload. When ServeFrontend boots under NEXUS_DEV=1,
// it mounts two extra routes:
//
//	GET /__nexus/dev/reload       SSE — broadcasts {} whenever a
//	                              file under NEXUS_DEV_ROOT changes
//	GET /__nexus/dev/script.js    tiny browser shim that connects
//	                              to the SSE and calls
//	                              location.reload() on each event
//
// emitIndexHTML in dev mode adds a `<script src=
// "/__nexus/dev/script.js">` tag before </body>, so every page
// served from a freshly-rebuilt bundle has the reload wiring
// automatically. Production binaries never set NEXUS_DEV=1, so
// none of this code path runs — zero runtime cost in prod.
//
// Why SSE instead of WebSockets: SSE survives every reverse proxy
// (no Upgrade handshake), auto-reconnects in the browser without
// any client code, and is one request shape we can debug with
// curl. The framework's existing /__nexus/events route uses the
// same pattern.

// devReloadHub is the broadcast fanout used by the SSE handler.
// Subscribers are buffered channels (cap 1) so a slow client
// never blocks the watcher's broadcast goroutine — when the
// channel is already full we drop the redundant signal because
// the slow client is going to reload anyway on the queued one.
type devReloadHub struct {
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func newDevReloadHub() *devReloadHub {
	return &devReloadHub{subscribers: map[chan struct{}]struct{}{}}
}

func (h *devReloadHub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subscribers[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *devReloadHub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subscribers, ch)
	h.mu.Unlock()
}

// broadcast wakes every subscriber. Non-blocking — a subscriber
// whose channel is full already has a reload pending, no benefit
// to queueing a second one.
func (h *devReloadHub) broadcast() {
	h.mu.Lock()
	for ch := range h.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

// mountDevReload registers the SSE + script.js routes on the
// engine and starts an fsnotify watcher rooted at watchDir.
// Called from mountFrontend ONLY when NEXUS_DEV=1.
//
// File changes within watchDir trigger a debounced broadcast:
// 80ms after the last event, all subscribers fire. The debounce
// coalesces the bursts esbuild typically emits (one .js + one
// .css + their .map siblings all land within milliseconds) into
// a single reload signal.
//
// Errors from the watcher are logged and swallowed; the dev
// loop should not crash the app if fsnotify hits a per-platform
// limit (e.g. macOS open-file cap).
func mountDevReload(engine *gin.Engine, watchDir string, exclude []string) {
	hub := newDevReloadHub()

	engine.GET("/__nexus/dev/reload", devReloadSSE(hub))
	engine.GET("/__nexus/dev/script.js", devReloadScript())

	if watchDir == "" {
		log.Printf("nexus: dev-reload: empty watch dir, SSE-only mode (no auto-broadcast)")
		return
	}
	// Validate operator-supplied ignore globs once, here, so a typo
	// surfaces at boot and the hot loop below never re-checks for
	// ErrBadPattern.
	exclude = validDevReloadGlobs(exclude)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("nexus: dev-reload: fsnotify init failed: %v (SSE mounted but no auto-broadcast)", err)
		return
	}
	// Recursive add: walk the directory at boot, register every
	// existing subdir. New subdirs created later are picked up
	// by the Create-event handling in the loop below.
	if err := addRecursive(w, watchDir); err != nil {
		log.Printf("nexus: dev-reload: watch %s: %v", watchDir, err)
		w.Close()
		return
	}

	go func() {
		defer w.Close()
		var debounce *time.Timer
		fire := func() {
			hub.broadcast()
		}
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if !devReloadRelevant(ev) || devReloadExcluded(ev.Name, watchDir, exclude) {
					continue
				}
				// New directory? Recursively add so changes inside
				// it also trigger reloads.
				if ev.Op&fsnotify.Create != 0 {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						_ = addRecursive(w, ev.Name)
					}
				}
				if debounce != nil {
					debounce.Stop()
				}
				debounce = time.AfterFunc(80*time.Millisecond, fire)
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("nexus: dev-reload: watcher: %v", err)
			}
		}
	}()
}

// addRecursive walks dir and adds every directory to w. fsnotify
// only watches per-dir on linux/darwin, so subdirs need explicit
// registration. Best-effort — unreadable subdirs (perms, sockets)
// are skipped silently rather than aborting the whole walk.
func addRecursive(w *fsnotify.Watcher, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		// Skip the usual noise dirs so we don't burn watch slots
		// on regenerated artifacts that don't affect the served
		// page (esbuild's .map files live next to the .js so
		// watching the dir is unavoidable; the per-file filter
		// in devReloadRelevant handles those).
		base := filepath.Base(path)
		if base == ".git" || base == "node_modules" {
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})
}

// devReloadRelevant filters out file events that don't warrant
// a browser reload. Skipped:
//
//   - .map files (sourcemaps refresh transparently via DevTools)
//   - hidden files (.DS_Store, editor swap files like .#foo.vue)
//   - runtime data the app writes itself (SQLite databases and
//     their WAL/journal sidecars, log files) — see
//     isRuntimeDataArtifact for why these must never reload
//   - directory-only events on a Chmod (perm bit changes don't
//     change page content)
//
// Everything else — write / create / rename / remove — flows
// through to broadcast.
func devReloadRelevant(ev fsnotify.Event) bool {
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") {
		return false
	}
	if strings.HasSuffix(base, ".map") {
		return false
	}
	if isRuntimeDataArtifact(base) {
		return false
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	return true
}

// isRuntimeDataArtifact reports whether base names a file the running
// app mutates as data rather than source — most commonly its own
// database. These never change the served frontend, and reloading on
// them risks an infinite loop: a request writes the DB → the watcher
// fires a reload → the reloaded page's bootstrap re-issues the request
// → it writes the DB again, forever. (A SQLite app that tracks
// per-request session state in the project root hits this the moment a
// user logs in.) Matched:
//
//   - SQLite databases: .db / .sqlite / .sqlite3
//   - SQLite sidecars: the -wal / -shm / -journal suffixes
//   - log files: .log
func isRuntimeDataArtifact(base string) bool {
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, ".db"),
		strings.HasSuffix(lower, ".sqlite"),
		strings.HasSuffix(lower, ".sqlite3"),
		strings.HasSuffix(lower, ".log"):
		return true
	case strings.HasSuffix(lower, "-wal"),
		strings.HasSuffix(lower, "-shm"),
		strings.HasSuffix(lower, "-journal"):
		return true
	}
	return false
}

// validDevReloadGlobs drops empty and malformed patterns from the
// operator's [runtime.devreload] exclude list, logging each bad one
// once so a config typo is visible at boot rather than silently
// swallowing changes. filepath.Match only errors on the pattern (not
// the input), so a single probe per pattern is sufficient.
func validDevReloadGlobs(patterns []string) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSuffix(filepath.ToSlash(strings.TrimSpace(p)), "/")
		if p == "" {
			continue
		}
		if _, err := filepath.Match(p, "probe"); err != nil {
			log.Printf("nexus: dev-reload: ignoring invalid exclude pattern %q: %v", p, err)
			continue
		}
		out = append(out, p)
	}
	return out
}

// devReloadExcluded reports whether the changed file at name (an
// absolute path inside watchDir) matches any operator-supplied ignore
// glob. Each pattern is tested three ways — a hit on any one excludes
// the file:
//
//   - against the base name           ("*.tmp" ignores foo.tmp anywhere)
//   - against the path relative to the
//     watch root                       ("cache/*.json")
//   - as a directory subtree prefix    ("uploads" ignores uploads/a/b)
//
// Patterns are assumed pre-validated by validDevReloadGlobs, so
// filepath.Match's error return is ignored here.
func devReloadExcluded(name, watchDir string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	base := filepath.Base(name)
	rel := base
	if r, err := filepath.Rel(watchDir, name); err == nil && !strings.HasPrefix(r, "..") {
		rel = filepath.ToSlash(r)
	}
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, base); ok {
			return true
		}
		if ok, _ := filepath.Match(p, rel); ok {
			return true
		}
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// devReloadSSE returns the long-lived SSE handler. Honors the
// client's Done channel so a closed browser tab releases the
// subscriber slot promptly.
//
// Sends a comment-line keepalive every 25s — some reverse
// proxies close idle SSE streams at 30-60s. The keepalive uses
// the `:` prefix so it doesn't trigger event listeners on the
// client side.
func devReloadSSE(hub *devReloadHub) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no") // disable nginx response buffering
		flusher, ok := c.Writer.(http.Flusher)
		if !ok {
			c.String(http.StatusInternalServerError, "streaming unsupported")
			return
		}
		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		fmt.Fprint(c.Writer, ": connected\n\n")
		flusher.Flush()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-ch:
				fmt.Fprint(c.Writer, "event: reload\ndata: {}\n\n")
				flusher.Flush()
			case <-keepalive.C:
				fmt.Fprint(c.Writer, ": ping\n\n")
				flusher.Flush()
			}
		}
	}
}

// devReloadScript serves the tiny JS shim injected into every
// page. Pure ES5 so it runs in any browser the operator might
// open the page in — the SPA's bundled code can target modern
// JS but the reload glue should be runtime-agnostic.
//
// Cache-Control: no-store so an inadvertent CDN / browser cache
// doesn't keep an old shim alive across an SDK upgrade.
func devReloadScript() gin.HandlerFunc {
	const body = `// nexus dev-reload shim — auto-injected by emitIndexHTML when NEXUS_DEV=1.
(function () {
  if (!window.EventSource) return;
  var url = '/__nexus/dev/reload';
  function connect() {
    var es = new EventSource(url);
    es.addEventListener('reload', function () {
      // Small delay so a burst of rebuilds (e.g. saving multiple
      // files in quick succession) only triggers one reload.
      setTimeout(function () { location.reload(); }, 50);
    });
    es.addEventListener('error', function () {
      // The browser auto-reconnects via EventSource's built-in
      // backoff; nothing to do here. Logging at info level
      // because a binary restart legitimately drops the stream
      // and reconnect resumes within a second.
      try { console.info('[nexus dev-reload] stream dropped — reconnecting'); } catch (e) {}
    });
  }
  connect();
})();
`
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", []byte(body))
	}
}

// devReloadScriptTag is the snippet injected into index.html by
// emitIndexHTML in dev mode. Exported so frontend_index.go can
// reuse without duplicating the literal.
const devReloadScriptTag = `<script src="/__nexus/dev/script.js"></script>`

// devReloadWatchDir returns the absolute path of the directory
// the dev-reload fsnotify watcher should track. Honors the same
// NEXUS_DEV_ROOT env var ServeFrontend reads in dev mode so the
// watcher and the disk-FS swap point at the same tree.
//
// Returns "" when neither the env var nor the working directory
// resolves — the caller treats "" as "skip the watcher, mount
// SSE-only". That gracefully degrades to a manual-reload
// experience instead of crashing the boot.
func devReloadWatchDir() string {
	root := os.Getenv(NexusDevRootEnv)
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return ""
	}
	return abs
}
