package nexus

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/paulmanoni/nexus/httpx"
)

// Dev-only live reload. When ServeFrontend boots under NEXUS_DEV=1,
// it mounts two extra routes:
//
//	GET /__nexus/dev/reload       SSE — opens with a `boot` event naming
//	                              this process, then sends `reload`
//	                              when the page is out of date and
//	                              nothing else will refresh it
//	GET /__nexus/dev/script.js    tiny browser shim that connects to
//	                              the SSE and reloads the page
//
// Two signals, each for the changes it can time correctly:
//
//   - A new process. Every process has a boot ID. The shim remembers
//     the one that served it; when its stream reconnects to a process
//     with a different ID, the app was rebuilt and the page reloads.
//     That is the trigger for Go changes: it fires once the new binary
//     is serving, not when the file is saved — under build-then-swap
//     the old binary is still answering then, and a save that does not
//     change the binary (or does not compile) never restarts it.
//   - A file event, for files the running process serves from disk
//     (a disk web/dist). Suppressed while a Vite dev server owns the
//     frontend (devServerLive): Vite applies those changes in place by
//     HMR, and a reload would throw that away. Go build inputs never
//     fire one — the boot ID covers them. A dev server starting or
//     stopping reloads once, onto the other mode (devReloadGate).
//
// A page opts in with `<script src="/__nexus/dev/script.js">`; the
// Inertia engine's dev head includes it. Production binaries never set
// NEXUS_DEV=1, so none of this code path runs — zero runtime cost in
// prod.
//
// Why SSE instead of WebSockets: SSE survives every reverse proxy
// (no Upgrade handshake), auto-reconnects in the browser without
// any client code, and is one request shape we can debug with
// curl. The framework's existing /__nexus/events route uses the
// same pattern.

// devBootID identifies this process to the reload shim. Random rather
// than the pid: a rebuilt binary can reuse a pid.
var devBootID = sync.OnceValue(func() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
})

// devReloadRetryMS is the reconnect delay the stream asks the browser for.
// The default is seconds; a build-then-swap is down for ~20ms, and the
// reload onto the new process waits for this reconnect.
const devReloadRetryMS = 250

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
// a single reload signal. devServer (the origin of a live Vite dev
// server named by the hot file, or NEXUS_VITE_DEV's value; "" when
// there is none) is consulted when the debounce fires, and polled for
// changes of owner; see devReloadGate.
//
// Errors from the watcher are logged and swallowed; the dev
// loop should not crash the app if fsnotify hits a per-platform
// limit (e.g. macOS open-file cap).
func mountDevReload(engine httpx.Router, watchDir string, exclude []string, devServer func() string) {
	hub := newDevReloadHub()

	engine.GET("/__nexus/dev/reload", devReloadSSE(hub))
	engine.GET("/__nexus/dev/script.js", devReloadScript())

	gate := newDevReloadGate(devServer)
	if devServer != nil {
		go func() {
			tick := time.NewTicker(devReloadPollInterval)
			defer tick.Stop()
			for range tick.C {
				if gate.ownerChanged() {
					hub.broadcast()
				}
			}
		}()
	}

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
			if gate.fileChanged() {
				hub.broadcast()
			}
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

// devReloadPollInterval is how often devReloadGate checks who owns the
// frontend. A change must hold for two checks before pages reload, so a
// dev server restarting (its hot file briefly absent) is not reported.
var devReloadPollInterval = 400 * time.Millisecond

// devReloadGate decides when file changes and dev-server state reload
// the page.
//
// File changes reload only while no dev server owns the frontend: while
// one does, it delivers those changes itself (HMR). Ownership changing —
// a dev server starting, stopping, or dying and leaving a stale hot file
// — reloads once, because the open pages were rendered for the other
// mode: built assets that will never hot-update, or modules from a
// server that is gone. That is polled rather than watched: a server
// killed with SIGKILL changes nothing on disk.
type devReloadGate struct {
	current func() string

	mu sync.Mutex
	// settled is the dev server the open pages were rendered for, by
	// origin — "" for none. Comparing origins rather than a live/not
	// flag also catches Vite coming back on a different port: open
	// pages would otherwise keep loading modules from the dead one,
	// and Vite's own client polls that dead origin forever.
	settled string
	seen    int // consecutive polls that disagreed with settled
}

func newDevReloadGate(current func() string) *devReloadGate {
	g := &devReloadGate{current: current}
	g.settled = g.owner()
	return g
}

func (g *devReloadGate) owner() string {
	if g.current == nil {
		return ""
	}
	return g.current()
}

// fileChanged reports whether a debounced batch of file changes reloads.
func (g *devReloadGate) fileChanged() bool { return g.owner() == "" }

// ownerChanged is called once per poll; it reports true when the owner
// has differed from the settled one for two polls in a row.
func (g *devReloadGate) ownerChanged() bool {
	now := g.owner()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now == g.settled {
		g.seen = 0
		return false
	}
	g.seen++
	if g.seen < 2 {
		return false
	}
	g.settled, g.seen = now, 0
	return true
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
//   - Go build inputs (.go, go.mod, go.sum, go.work) — see
//     isGoBuildInput
//   - anything under a .vite directory: Vite's own metadata (the build
//     manifest, which lands beside the assets that do reload; the dev
//     hot file, whose state devReloadGate polls; a cacheDir)
//
// Everything else — write / create / rename / remove — flows
// through to broadcast.
func devReloadRelevant(ev fsnotify.Event) bool {
	base := filepath.Base(ev.Name)
	if strings.HasPrefix(base, ".") || inViteDir(ev.Name) {
		return false
	}
	if strings.HasSuffix(base, ".map") {
		return false
	}
	if isRuntimeDataArtifact(base) || isGoBuildInput(base) {
		return false
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
		return false
	}
	return true
}

// inViteDir reports whether name has a .vite directory among its parents.
func inViteDir(name string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(filepath.Dir(name)), "/") {
		if seg == ".vite" {
			return true
		}
	}
	return false
}

// isGoBuildInput reports whether base names a file that changes the app
// only by being compiled into it. The running process cannot show such a
// change, so reloading on the save only re-renders the old binary — under
// build-then-swap it is still serving while the new one compiles, and
// keeps serving if the save does not compile or leaves the binary
// unchanged. The page reloads when the rebuilt process answers instead,
// through the boot ID the SSE stream opens with.
func isGoBuildInput(base string) bool {
	switch base {
	case "go.mod", "go.sum", "go.work", "go.work.sum":
		return true
	}
	return strings.HasSuffix(base, ".go")
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
// Every stream opens with the reconnect delay and a `boot` event naming
// this process (devBootID) — how the shim tells a reconnect to a
// rebuilt app from one to the same process.
//
// Sends a comment-line keepalive every 25s — some reverse
// proxies close idle SSE streams at 30-60s. The keepalive uses
// the `:` prefix so it doesn't trigger event listeners on the
// client side.
func devReloadSSE(hub *devReloadHub) httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no") // disable nginx response buffering
		// *httpx.ResponseWriter forwards Flush() to the backend writer
		// (a no-op if the backend can't stream).
		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		fmt.Fprintf(c.Writer, "retry: %d\nevent: boot\ndata: {\"id\":%q}\n\n", devReloadRetryMS, devBootID())
		c.Writer.Flush()

		keepalive := time.NewTicker(25 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-c.Request.Context().Done():
				return
			case <-ch:
				fmt.Fprint(c.Writer, "event: reload\ndata: {}\n\n")
				c.Writer.Flush()
			case <-keepalive.C:
				fmt.Fprint(c.Writer, ": ping\n\n")
				c.Writer.Flush()
			}
		}
	}
}

// devReloadShim is the browser half, served by devReloadScript with the
// serving process's boot ID in place of devReloadBootMarker. Pure ES5 so it runs in any
// browser the operator might open the page in — the SPA's bundled code
// can target modern JS but the reload glue should be runtime-agnostic.
//
// The ID baked into the script is the process that rendered the page (the
// two are fetched together), so a page served just before a swap whose
// stream first connects to the new process still reloads. EventSource
// reconnects by itself after a network error; one it has given up on —
// an error status from a half-started process — is reopened here with
// backoff. A second copy of the shim on the same page is a no-op.
const devReloadShim = `// nexus dev-reload shim — auto-injected when NEXUS_DEV=1.
(function () {
  if (!window.EventSource || window.__nexusDevReload) return;
  window.__nexusDevReload = true;
  var url = '/__nexus/dev/reload';
  var boot = __NEXUS_BOOT_ID__;
  var delay = 250, reloading = false;
  function reload() {
    if (reloading) return;
    reloading = true;
    // Short delay so a burst of changes triggers one reload.
    setTimeout(function () { location.reload(); }, 50);
  }
  function connect() {
    var es = new EventSource(url);
    es.addEventListener('boot', function (e) {
      delay = 250;
      var id = '';
      try { id = JSON.parse(e.data).id || ''; } catch (err) {}
      if (!id) return;
      if (!boot) boot = id;
      else if (id !== boot) reload();
    });
    es.addEventListener('reload', reload);
    es.addEventListener('error', function () {
      try { console.info('[nexus dev-reload] stream dropped — reconnecting'); } catch (err) {}
      if (es.readyState !== 2) return;
      es.close();
      setTimeout(connect, delay);
      delay = Math.min(delay * 2, 5000);
    });
  }
  connect();
})();
`

const devReloadBootMarker = "__NEXUS_BOOT_ID__"

// devReloadScript serves the shim to the dev pages that load it.
//
// Cache-Control: no-store so an inadvertent CDN / browser cache
// doesn't keep an old shim — or an old boot ID — alive.
func devReloadScript() httpx.HandlerFunc {
	return func(c *httpx.Ctx) {
		body := strings.Replace(devReloadShim, devReloadBootMarker, strconv.Quote(devBootID()), 1)
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "application/javascript; charset=utf-8", []byte(body))
	}
}

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
