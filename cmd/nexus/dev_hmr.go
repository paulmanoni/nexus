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
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
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
//	{"type":"vue-update","id":"data-v-abc","url":"http://.../update/x.js","kind":"rerender"}
//	{"type":"reload"}
type hmrMessage struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	CSS  string `json:"css,omitempty"`
	// URL + Kind carry a Stage 2c vue-update: URL is the loopback
	// address of the freshly-built update module; Kind is "rerender"
	// (template-only change) or "reload" (script change), selecting which
	// Vue HMR runtime call the client makes.
	URL  string `json:"url,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// hmrServer is the SSE fanout + client-script host for dev HMR.
type hmrServer struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
	addr    string // host:port the server actually bound to

	// buildGen counts completed rebuilds. The bundler's OnRebuild bumps
	// it AFTER the fresh bundle (+ index.html + preserved chunks) is on
	// disk. The source watcher waits on it so a reload is broadcast only
	// once the new bundle exists — otherwise the browser reloads into the
	// PREVIOUS build ("reload shows the previous change"). The watcher is
	// the single decision-maker (only it can tell a CSS-only edit, which
	// hot-swaps without reload, from a reload-needing one), so OnRebuild
	// never broadcasts a reload itself.
	buildGen atomic.Int64

	// updates holds per-edit Vue HMR update modules (Stage 2c), keyed by
	// their URL path, served by the /__nexus_hmr/update/ handler. A miss
	// (stale generation) just 404s, and the client falls back to reload.
	updates *updateModuleCache
}

// bumpBuild signals a rebuild finished with its output on disk.
func (h *hmrServer) bumpBuild() { h.buildGen.Add(1) }

// waitBuildAfter blocks until buildGen advances past prev (a rebuild
// completed after the caller's reference point) or timeout elapses.
// Polls rather than using a cond var to keep the watcher and bundler
// goroutines loosely coupled; the poll only runs on a reload-needing
// edit and is cheap.
func (h *hmrServer) waitBuildAfter(prev int64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for h.buildGen.Load() <= prev {
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// vueBridgeJS is auto-imported into every dev entry (via esbuild Inject).
// It pins the app's own Vue instance onto globalThis.__nexus_vue__ so a
// separately-served HMR update module can bind to the SAME instance —
// Vue's HMR runtime requires single-instance vnode identity, and a 2nd
// bundled Vue would silently break rerender/reload. The `import "vue"` is
// a bare spec, so esbuild dedups it to the instance the app already
// bundles. Dev-only: prod never sets Inject, so this never ships.
const vueBridgeJS = `import * as __nexusVue from "vue";
if (typeof globalThis !== "undefined" && !globalThis.__nexus_vue__) {
  globalThis.__nexus_vue__ = __nexusVue;
}
`

// writeVueBridge materializes vueBridgeJS to a stable path under outDir's
// parent (NOT inside outDir — it must not be embedded/served) and returns
// the absolute path for esbuild's Inject. esbuild Inject needs a real
// file on disk, so we write it once per dev session.
func writeVueBridge(root string) (string, error) {
	dir := filepath.Join(root, ".nexus-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "vue-hmr-bridge.js")
	if err := os.WriteFile(p, []byte(vueBridgeJS), 0o644); err != nil {
		return "", err
	}
	return p, nil
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
	h := &hmrServer{
		clients: make(map[chan []byte]struct{}),
		addr:    ln.Addr().String(),
		updates: newUpdateModuleCache(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__nexus_hmr/sse", h.handleSSE)
	mux.HandleFunc("/__nexus_hmr/client.js", h.handleClient)
	mux.HandleFunc("/__nexus_hmr/update/", h.handleUpdate)

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

// handleUpdate serves a per-edit Vue HMR update module (Stage 2c). The
// browser import()s the URL from a vue-update SSE message. A cache miss
// (stale generation, or updates never populated) returns 404, which the
// client treats as "fall back to full reload".
func (h *hmrServer) handleUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if h.updates == nil {
		http.NotFound(w, r)
		return
	}
	js, ok := h.updates.get(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	fmt.Fprint(w, js)
}

// devVueRewrite returns a resolver.Options.DevSpecRewrite that swaps the
// bare `vue` import for its `.development.mjs` esm.sh build, so the Vue
// HMR runtime (__VUE_HMR_RUNTIME__, createRecord, rerender) — which is
// stripped from the production build esm.sh serves by default — is
// present in the dev bundle. Required groundwork for state-preserving
// Vue HMR.
//
// Only the top-level `vue` spec is rewritten: esm.sh's vue.development.mjs
// re-exports through runtime-dom.development.mjs → runtime-core.development.mjs,
// and those transitive imports resolve through the resolver's normal
// registry-internal path (already .development.mjs URLs), so the whole
// dev chain follows from this one swap. Packages built with
// `?external=vue` emit bare `import … from "vue"`, which also hits this
// rewrite — so every consumer shares ONE dev Vue instance (mismatched
// prod/dev instances would break reactivity).
//
// The version is read from the lockfile's pinned `vue` so the dev build
// matches exactly what production would use. Returns nil when vue isn't
// in the lockfile (non-Vue project) → no rewriting.
//
// CONCURRENCY: the dev URL is PRE-WARMED here (single-threaded, before
// esbuild's parallel build starts) via onDemand, which populates the
// store + lockfile. esbuild then fans OnResolve across goroutines; if
// the rewrite triggered onDemand (→ lockfile map write) on the hot path
// it would race the concurrent lf.Resolve reads ("concurrent map
// iteration and map write"). Pre-warming means the resolver's dev block
// hits a warm Store.Get and never writes the lockfile mid-build.
func devVueRewrite(lf *lockfile.File, onDemand func(string) (string, error), stdout io.Writer) func(spec, subpath string) string {
	pkg, err := lf.Resolve("vue", "")
	if err != nil || pkg.Version == "" {
		return nil
	}
	devURL := "https://esm.sh/vue@" + pkg.Version + "/es2022/vue.development.mjs"
	// Return the CANONICAL (post-redirect) URL the pre-warm actually
	// stored under, not the raw request URL — esm.sh may 301 to a
	// different cache key, and the resolver's read-only hot path looks
	// up exactly this string.
	resolved := devURL
	if onDemand != nil {
		canonical, ferr := onDemand(devURL)
		if ferr != nil {
			// Pre-warm failed (offline / esm.sh hiccup). Skip the
			// rewrite entirely so the build falls back to the pinned
			// production vue rather than failing — HMR just won't be
			// available this session.
			fmt.Fprintf(stdout, "%s[hmr]%s vue dev build prefetch failed (%v); Vue HMR disabled, using production vue\n", ansiYellow, ansiReset, ferr)
			return nil
		}
		if canonical != "" {
			resolved = canonical
		}
	}
	return func(spec, subpath string) string {
		if spec == "vue" && subpath == "" {
			return resolved
		}
		return ""
	}
}

// preserveDevChunks keeps every code-split chunk this dev session has
// ever emitted available on disk, even after esbuild's persistent-watch
// rebuild deletes the old-hash files.
//
// Why: lazy routes compile to content-hashed chunks
// (chunks/RouteView-HASH.js). On rebuild, esbuild writes new-hash files
// and GARBAGE-COLLECTS the old ones. A live page that hot-swapped (CSS
// HMR, no reload) still holds dynamic import() URLs pointing at the OLD
// hash; once that file is gone, navigating to a not-yet-loaded route
// 404s — which the SPA server returns as text/plain, so the browser
// rejects it as a module ("disallowed MIME type"). Symptom: every
// unvisited route dies after any edit.
//
// Fix: maintain a session archive (in the OS temp dir, keyed by outDir)
// and on each rebuild (1) copy current chunks into the archive, then
// (2) restore any archived chunk missing from the live dir. Both old and
// new hashes coexist, so stale import() URLs keep resolving while fresh
// page loads get the new graph. Content hashing is preserved, so there's
// no anonymous-chunk name collision.
//
// Dev-only: the archive lives outside outDir (no embed/serve pollution),
// and `nexus build` rebuilds islands/ from scratch so production is
// unaffected. Stale chunks accrue only for the session.
func preserveDevChunks(outDir string) {
	chunksDir := filepath.Join(outDir, "chunks")
	if _, err := os.Stat(chunksDir); err != nil {
		return // no split chunks (single-entry build) — nothing to do
	}
	sum := sha256.Sum256([]byte(outDir))
	cacheDir := filepath.Join(os.TempDir(), "nexus-devchunks", hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}

	live := map[string]bool{}
	if entries, err := os.ReadDir(chunksDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			live[e.Name()] = true
			// Archive the current chunk. Cheap content-skip: a
			// content-hashed name that already exists in the archive
			// has identical bytes, so don't recopy.
			dst := filepath.Join(cacheDir, e.Name())
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			_ = copyDevChunk(filepath.Join(chunksDir, e.Name()), dst)
		}
	}

	// Restore any archived chunk esbuild deleted but a live page may
	// still import.
	if cached, err := os.ReadDir(cacheDir); err == nil {
		for _, e := range cached {
			if e.IsDir() || live[e.Name()] {
				continue
			}
			_ = copyDevChunk(filepath.Join(cacheDir, e.Name()), filepath.Join(chunksDir, e.Name()))
		}
	}
}

// copyDevChunk copies src to dst (best-effort, atomic-ish via temp +
// rename so a concurrently-loading browser never reads a half-written
// chunk).
func copyDevChunk(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
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
func startHMRSourceWatcher(ctx context.Context, srcDir string, compiler vue.SFCCompiler, hub *hmrServer, deps updateBuildDeps, stdout io.Writer) {
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

	var updateGen int64 // monotonic per-session counter for update URLs
	fire := func(path string, prevGen int64) {
		pmu.Lock()
		delete(pending, path)
		pmu.Unlock()
		msg, code := classifyChange(compiler, path, cache)

		switch msg.Type {
		case "css":
			// CSS hot-swap is bundle-independent (the compiled CSS rides
			// in the message), so broadcast immediately — no reload.
			hub.broadcast(msg)
			fmt.Fprintf(stdout, "%s[hmr]%s css %s\n", ansiCyan, ansiReset, filepath.Base(path))
			return

		case "vue-update":
			// Stage 2c: build a standalone update module (Vue externalized
			// to the app's instance), cache + serve it, and tell the
			// browser to swap the component in place. Self-contained, so
			// no need to wait on the full rebuild. Any failure falls
			// through to a full reload.
			js, berr := buildVueUpdateModule(code, path, deps)
			if berr == nil && js != "" && hub.updates != nil {
				updateGen++
				urlPath := fmt.Sprintf("/__nexus_hmr/update/%s-%d.js", msg.ID, updateGen)
				hub.updates.put(urlPath, js)
				msg.URL = "http://" + hub.addr + urlPath
				hub.broadcast(msg)
				fmt.Fprintf(stdout, "%s[hmr]%s update %s\n", ansiCyan, ansiReset, filepath.Base(path))
				return
			}
			fmt.Fprintf(stdout, "%s[hmr]%s update build failed for %s, reloading: %v\n", ansiYellow, ansiReset, filepath.Base(path), berr)
			// fall through to reload
		}

		// Reload-needing change: wait until esbuild's rebuild lands on
		// disk (buildGen advances past the value seen when this edit
		// arrived) BEFORE telling the browser to reload — otherwise it
		// reloads into the PREVIOUS bundle. Timeout guards a stalled or
		// failed build; reload anyway so the user isn't stuck.
		hub.waitBuildAfter(prevGen, 15*time.Second)
		hub.broadcast(hmrMessage{Type: "reload"})
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
			// Capture the build generation at EVENT time so a reload for
			// this edit waits for a rebuild completing AFTER this point.
			prevGen := hub.buildGen.Load()
			pmu.Lock()
			if t, ok := pending[path]; ok {
				t.Stop()
			}
			pending[path] = time.AfterFunc(debounce, func() { fire(path, prevGen) })
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
func classifyChange(compiler vue.SFCCompiler, path string, cache map[string]vueSig) (hmrMessage, string) {
	if compiler == nil || strings.ToLower(filepath.Ext(path)) != ".vue" {
		return hmrMessage{Type: "reload"}, ""
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return hmrMessage{Type: "reload"}, ""
	}
	res, err := compiler.Compile(string(src), path)
	if err != nil || len(res.Errors) > 0 {
		// Compile error → reload so esbuild's error overlay / a fresh
		// load surfaces it; don't try to hot-swap broken output.
		return hmrMessage{Type: "reload"}, ""
	}
	cur := fingerprintVue(res.Code)
	prev, had := cache[path]
	cache[path] = cur

	// CSS-only change: logic identical, style changed, and no url()
	// interpolation we can't resolve outside the bundle → hot-swap.
	if had && cur.nonStyle == prev.nonStyle {
		if cur.css != prev.css && !cur.hasInterp && cur.css != "" {
			return hmrMessage{Type: "css", ID: cur.id, CSS: cur.css}, res.Code
		}
		// Nothing meaningfully changed. This is the common duplicate
		// fsnotify event after a real edit already handled — return a
		// no-op so we don't fire a spurious reload that would undo the
		// vue-update/css-swap the first event produced.
		return hmrMessage{Type: "noop"}, ""
	}

	// Template/script change (or first sight): vue-update. The caller
	// builds + serves the update module and fills in URL. Needs a scope
	// id to target the component; Kind "reload" drives Vue's HMR reload()
	// which handles both template and script changes.
	if cur.id == "" {
		return hmrMessage{Type: "reload"}, ""
	}
	return hmrMessage{Type: "vue-update", ID: cur.id, Kind: "reload"}, res.Code
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
  function applyVueUpdate(msg) {
    var R = globalThis.__VUE_HMR_RUNTIME__;
    if (!R) { location.reload(); return; }
    import(msg.url).then(function (mod) {
      var comp = mod && (mod.default || mod);
      try {
        if (msg.kind === 'rerender' && comp && comp.render) {
          R.rerender(msg.id, comp.render);
        } else if (comp) {
          R.reload(msg.id, comp);
        } else {
          location.reload();
          return;
        }
        console.debug('[nexus-hmr] component updated', msg.id);
      } catch (err) {
        console.warn('[nexus-hmr] update failed, reloading', err);
        location.reload();
      }
    }).catch(function (err) {
      console.warn('[nexus-hmr] update fetch failed, reloading', err);
      location.reload();
    });
  }
  function connect() {
    var es = new EventSource(base + '/__nexus_hmr/sse');
    es.onmessage = function (e) {
      var msg;
      try { msg = JSON.parse(e.data); } catch (_) { return; }
      if (msg.type === 'css') { applyCss(msg.id, msg.css); return; }
      if (msg.type === 'vue-update') { applyVueUpdate(msg); return; }
      if (msg.type === 'reload') { location.reload(); return; }
    };
    es.onerror = function () { /* EventSource retries automatically */ };
  }
  connect();
})();`
