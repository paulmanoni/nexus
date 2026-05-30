package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
)

// Stage-1 HMR: CSS hot-swap. The dev watcher hosts a tiny SSE server
// on a loopback port and injects a client shim (served from the same
// server) into the dev index.html. On a source change the watcher
// classifies it and pushes either a targeted CSS update (swap the
// matching <style data-nl-sfc=...> in place, no reload) or a plain
// reload. Everything lives in the CLI process — no runtime-module
// change, no app-side coordination — so the browser talks only to
// this server in dev.
//
// Why a separate server instead of the runtime's existing
// /__nexus/dev/* SSE: that endpoint only knows "something on disk
// changed" (it watches the bundled output). CSS-vs-JS classification
// needs SOURCE-level knowledge, which only the watcher (with the SFC
// compiler) has. Driving all dev events from here keeps a single
// decision-maker and avoids a double-fire race with the app's reload.

// hmrMessage is the JSON payload pushed to the browser client.
//
//	{"type":"css","id":"data-v-abc","css":"...compiled css..."}
//	{"type":"reload"}
type hmrMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	CSS  string `json:"css,omitempty"`
}

// hmrServer is the SSE fanout + client-script host for dev HMR.
type hmrServer struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	addr    string // host:port the server actually bound to
}

// startHMRServer binds a loopback SSE server on an OS-assigned port
// and serves the client shim. Returns the server (for broadcasting)
// and the base URL the injected <script> should point at, or an
// error if it couldn't bind. The listener is closed when ctx-driven
// shutdown closes the process; for the dev loop's lifetime it stays
// up.
func startHMRServer() (*hmrServer, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("hmr: listen: %w", err)
	}
	h := &hmrServer{clients: make(map[chan []byte]struct{}), addr: ln.Addr().String()}

	mux := http.NewServeMux()
	mux.HandleFunc("/__nexus_hmr/sse", h.handleSSE)
	mux.HandleFunc("/__nexus_hmr/client.js", h.handleClient)

	go func() { _ = http.Serve(ln, mux) }()

	base := "http://" + h.addr
	return h, base, nil
}

// broadcast fans a message out to every connected client. Slow
// clients (full channel) are skipped for this message — they'll get
// the next one, and a missed CSS frame just means a stale style for a
// moment, not a broken loop.
func (h *hmrServer) broadcast(msg hmrMessage) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *hmrServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The page is served from the app's origin (e.g. :9590) while this
	// SSE lives on a loopback port — cross-origin, so allow it.
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, ch)
		h.mu.Unlock()
	}()

	// Initial comment flushes headers so the client's onopen fires.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

func (h *hmrServer) handleClient(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, hmrClientJS, "http://"+h.addr)
}

// swapInHMRClient rewrites the on-disk dev index.html so it loads the
// HMR client (from this CLI's loopback server) instead of the app's
// reload shim. Replacing the tag — rather than adding ours alongside —
// means the page connects ONLY to our SSE, so the app's output-dir
// reload watcher can't double-fire and undo a CSS hot-swap.
//
// Idempotent: if our client is already present it's a no-op; if the
// app tag is absent we inject before </body>.
func swapInHMRClient(outDir, hmrBase string) error {
	p := filepath.Join(outDir, "index.html")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	s := string(b)
	clientTag := `<script src="` + hmrBase + `/__nexus_hmr/client.js"></script>`
	switch {
	case strings.Contains(s, "/__nexus_hmr/client.js"):
		return nil // already injected
	case strings.Contains(s, devReloadScriptTag):
		s = strings.Replace(s, devReloadScriptTag, clientTag, 1)
	default:
		if i := strings.LastIndex(strings.ToLower(s), "</body>"); i >= 0 {
			s = s[:i] + "  " + clientTag + "\n  " + s[i:]
		} else {
			s = s + "\n" + clientTag + "\n"
		}
	}
	return os.WriteFile(p, []byte(s), 0o644)
}

// vueSig is the cached per-.vue fingerprint the classifier diffs
// across edits to decide CSS-only vs full-reload.
type vueSig struct {
	id        string // scope id (data-v-...), the <style data-nl-sfc> target
	nonStyle  string // hash of everything in the compiled module except __css
	css       string // the compiled, scoped CSS (unescaped from the template literal)
	hasInterp bool   // css carries ${...} url() interpolations → not safe to hot-swap
}

// startHMRSourceWatcher watches srcDir recursively for frontend source
// edits and drives the HMR hub. .vue edits are classified (CSS-only →
// targeted css swap; anything else → reload); other source files fall
// back to reload. Blocks until ctx is cancelled.
//
// Runs ALONGSIDE esbuild's own watcher: esbuild still rebuilds the
// on-disk bundle (so a later full reload or a fresh page load is
// correct), while this loop decides whether the browser needs a
// reload at all or just a style swap.
func startHMRSourceWatcher(ctx context.Context, srcDir string, compiler vue.SFCCompiler, hub *hmrServer, stdout io.Writer) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(stdout, "%s●%s hmr: fsnotify init failed: %v (CSS hot-swap disabled)\n", ansiYellow, ansiReset, err)
		return
	}
	defer w.Close()
	if err := hmrAddRecursive(w, srcDir); err != nil {
		fmt.Fprintf(stdout, "%s●%s hmr: watch %s: %v\n", ansiYellow, ansiReset, srcDir, err)
	}

	cache := map[string]vueSig{}
	// Debounce: editors emit several events per save; coalesce a burst
	// per-path within a short window before classifying.
	const debounce = 80 * time.Millisecond
	pending := map[string]*time.Timer{}
	var pmu sync.Mutex

	fire := func(path string) {
		pmu.Lock()
		delete(pending, path)
		pmu.Unlock()
		msg := classifyChange(compiler, path, cache)
		hub.broadcast(msg)
		if msg.Type == "css" {
			fmt.Fprintf(stdout, "%s[hmr]%s css %s\n", ansiCyan, ansiReset, filepath.Base(path))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			// Newly created directory → start watching it too.
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = hmrAddRecursive(w, ev.Name)
					continue
				}
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}
			if !isHMRSource(ev.Name) {
				continue
			}
			path := ev.Name
			pmu.Lock()
			if t, ok := pending[path]; ok {
				t.Stop()
			}
			pending[path] = time.AfterFunc(debounce, func() { fire(path) })
			pmu.Unlock()
		case _, ok := <-w.Errors:
			if !ok {
				return
			}
		}
	}
}

// isHMRSource reports whether path is a frontend source file the
// watcher should react to.
func isHMRSource(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".vue", ".css", ".scss", ".sass", ".ts", ".tsx", ".js", ".jsx":
		return true
	}
	return false
}

// classifyChange decides what HMR message a changed file warrants and
// updates the cache. .vue files are compiled and diffed: if only the
// compiled CSS changed (and it has no url() interpolations we can't
// resolve here), a targeted css swap is returned; otherwise reload.
// Non-.vue files always reload in Stage 1.
func classifyChange(compiler vue.SFCCompiler, path string, cache map[string]vueSig) hmrMessage {
	if compiler == nil || strings.ToLower(filepath.Ext(path)) != ".vue" {
		return hmrMessage{Type: "reload"}
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return hmrMessage{Type: "reload"}
	}
	res, err := compiler.Compile(string(src), path)
	if err != nil || len(res.Errors) > 0 {
		// Compile error → reload so esbuild's error overlay / a fresh
		// load surfaces it; don't try to hot-swap broken output.
		return hmrMessage{Type: "reload"}
	}
	cur := fingerprintVue(res.Code)
	prev, had := cache[path]
	cache[path] = cur

	// First time we see this file, or the non-CSS parts changed, or
	// the CSS uses url() interpolations we can't resolve here → reload.
	if !had || cur.nonStyle != prev.nonStyle || cur.hasInterp || cur.css == "" {
		return hmrMessage{Type: "reload"}
	}
	if cur.css == prev.css {
		// Nothing visible changed (e.g. whitespace-only in script that
		// hashed identically is impossible, but CSS identical means the
		// edit was elsewhere and already handled by nonStyle compare).
		return hmrMessage{Type: "reload"}
	}
	return hmrMessage{Type: "css", ID: cur.id, CSS: cur.css}
}

// fingerprintVue splits a compiled SFC module into its CSS and
// everything-else, returning the pieces the classifier diffs.
func fingerprintVue(code string) vueSig {
	id := extractBetween(code, `__sfc__.__scopeId = "`, `"`)
	css, hasInterp := extractCSSLiteral(code)
	// nonStyle = hash of the module with the __css literal elided, so
	// template/script/import edits register but CSS edits don't.
	nonStyle := code
	if i := strings.Index(code, "const __css = "); i >= 0 {
		if start, end := cssLiteralSpan(code, i); end > start {
			nonStyle = code[:start] + code[end:]
		}
	}
	sum := sha256.Sum256([]byte(nonStyle))
	return vueSig{id: id, nonStyle: hex.EncodeToString(sum[:]), css: css, hasInterp: hasInterp}
}

// extractBetween returns the substring between the first occurrence of
// open and the next close after it, or "" if not found.
func extractBetween(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// cssLiteralSpan returns the [start,end) byte span of the
// `const __css = ` + backtick-literal + `;` statement, given the index
// of "const __css = ". end is just past the closing backtick. Returns
// (0,0) if the literal can't be located.
func cssLiteralSpan(code string, declIdx int) (int, int) {
	p := declIdx + len("const __css = ")
	if p >= len(code) || code[p] != '`' {
		return 0, 0
	}
	// scan to the matching unescaped backtick
	i := p + 1
	for i < len(code) {
		switch code[i] {
		case '\\':
			i += 2
			continue
		case '`':
			return declIdx, i + 1
		}
		i++
	}
	return 0, 0
}

// extractCSSLiteral pulls the compiled CSS out of `const __css = ` + a
// backtick template literal. Returns the unescaped CSS and whether it
// contains ${...} interpolations (url() asset imports) that can't be
// resolved outside the bundle — in which case it isn't safe to hot-swap.
func extractCSSLiteral(code string) (string, bool) {
	i := strings.Index(code, "const __css = ")
	if i < 0 {
		return "", false
	}
	start, end := cssLiteralSpan(code, i)
	if end <= start {
		return "", false
	}
	// inner = between the backticks
	inner := code[start+len("const __css = `") : end-1]
	if strings.Contains(inner, "${") {
		return unescapeTemplate(inner), true
	}
	return unescapeTemplate(inner), false
}

// unescapeTemplate reverses adapter.js escTemplate: \\ → \, \` → `,
// \$ → $. Single left-to-right scan so escape sequences don't
// re-trigger.
func unescapeTemplate(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\', '`', '$':
				b.WriteByte(s[i+1])
				i++
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// hmrAddRecursive adds dir and all its subdirectories to the watcher,
// skipping the usual noise dirs so we don't burn watch descriptors.
func hmrAddRecursive(w *fsnotify.Watcher, dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		switch info.Name() {
		case "node_modules", ".git", "dist", "islands":
			return filepath.SkipDir
		}
		_ = w.Add(path)
		return nil
	})
}

// hmrClientJS is the browser shim. %s is the server base URL. It
// connects over SSE and, per message:
//   - css: replaces the textContent of <style data-nl-sfc="<id>">
//     in place (creating it if somehow absent) — no reload, state
//     preserved.
//   - reload: full page reload (fallback for anything not hot-swappable).
//
// EventSource auto-reconnects on drop, so a watcher restart or a brief
// disconnect re-establishes the stream without user action.
const hmrClientJS = `(function () {
  var base = %q;
  function applyCss(id, css) {
    var el = document.querySelector('style[data-nl-sfc="' + id + '"]');
    if (!el) {
      el = document.createElement('style');
      el.setAttribute('data-nl-sfc', id);
      document.head.appendChild(el);
    }
    el.textContent = css;
    console.debug('[nexus-hmr] css updated', id);
  }
  function connect() {
    var es = new EventSource(base + '/__nexus_hmr/sse');
    es.onmessage = function (e) {
      var msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      if (msg.type === 'css') { applyCss(msg.id, msg.css); return; }
      if (msg.type === 'reload') { location.reload(); return; }
    };
    es.onerror = function () { /* EventSource retries automatically */ };
  }
  connect();
})();`