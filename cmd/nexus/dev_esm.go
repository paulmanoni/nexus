package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/sfc/vue"
	"github.com/paulmanoni/nexus/frontend/deps/store"
	"github.com/paulmanoni/nexus/frontend/devserver"
	"github.com/paulmanoni/viteless"
)

// startESMWatcher runs the unbundled, native-ESM dev server (the viteless
// architecture) instead of the esbuild bundler watcher. It serves the SPA
// one module per URL — so every dependency resolves to ONE URL and shares a
// single Vue instance, which is what makes state-preserving HMR work — and
// reverse-proxies anything it doesn't serve to the Go app so the browser
// stays on one origin.
//
// It's opt-in (NEXUS_DEV_ESM): the bundler watcher remains the default and
// `nexus build` is untouched. appAddr is the Go app's bind address, used as
// the proxy target. The dev URL is reported on frontendURLCh so dev.go opens
// the viteless origin rather than the app's.
func startESMWatcher(ctx context.Context, dir, appAddr string, verbose bool, stdout, stderr io.Writer, frontendURLCh chan<- string) error {
	root, err := findProjectRoot(dir)
	if err != nil {
		return fmt.Errorf("esm dev: %w", err)
	}

	srcName := islandsSrcName()
	srcDir := filepath.Join(root, srcName)
	actualSrcDir, entries, err := waitForFrontendEntries(ctx, srcDir, srcName, stdout)
	if err != nil {
		return err
	}
	if entries == nil {
		return nil // ctx cancelled during wait
	}

	// The served origin Root is the directory that holds index.html (Vite
	// convention: index.html at the frontend root, sources under src/).
	// Fall back to the entries' directory if the conventional spot has no
	// shell.
	servedRoot := srcDir
	if _, e := os.Stat(filepath.Join(srcDir, "index.html")); e != nil {
		if _, e2 := os.Stat(filepath.Join(actualSrcDir, "index.html")); e2 == nil {
			servedRoot = actualSrcDir
		}
	}

	hasVue, err := hasVueSources(actualSrcDir)
	if err != nil {
		return fmt.Errorf("esm dev: scan vue sources: %w", err)
	}
	if hasVue && vueCompilerHook == nil {
		return fmt.Errorf("esm dev: .vue sources detected but no SFC compiler is wired — " +
			"set CGO_ENABLED=1 with `-tags vue`, or drop the tag for the WASM backend")
	}

	lockPath := filepath.Join(root, lockfile.Filename)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return fmt.Errorf("esm dev: load lockfile: %w", err)
	}

	cacheRoot := os.Getenv("NEXUS_CACHE")
	if cacheRoot == "" {
		cacheRoot = store.DefaultRoot()
	}
	st, err := store.New(cacheRoot)
	if err != nil {
		return fmt.Errorf("esm dev: open cache %s: %w", cacheRoot, err)
	}

	// Pass an empty lockPath so on-demand fetches populate the in-memory
	// lockfile + the shared cache for THIS session but never rewrite the
	// project's committed nexus.lock. The dev server pulls dev-only URLs
	// (vue.development.mjs and friends) that must not leak into the
	// checked-in lockfile — and a 2nd `vue` entry there would make
	// lf.Resolve("vue") ambiguous on the next run, blanking the import.
	onDemand := makeOnDemandFetch(lf, st, "", stdout)
	resOpts := resolver.Options{
		Lockfile:       lf,
		Store:          st,
		FetchOnDemand:  onDemand,
		DevSpecRewrite: devVueRewrite(lf, onDemand, stdout),
	}

	// Vue SFC compiler — same hook the bundler uses. Lifetime tracks ctx.
	var compiler vue.SFCCompiler
	var closeVue func()
	if hasVue && vueCompilerHook != nil {
		cv, _, c, verr := vueCompilerHook(lf, st)
		if verr != nil {
			return fmt.Errorf("esm dev: vue compiler: %w", verr)
		}
		compiler, closeVue = c, cv
	}

	envSink := stdout
	if !verbose {
		envSink = io.Discard
	}
	viteEnv, envErr := loadViteEnv(root, "development", envSink)
	if envErr != nil {
		fmt.Fprintf(stderr, "%s●%s esm dev: .env load: %v\n", ansiYellow, ansiReset, envErr)
	}

	host := devserver.New(devserver.Config{
		Root:      servedRoot,
		Resolver:  resOpts,
		Compiler:  compiler,
		Aliases:   esmAliases(servedRoot),
		Env:       viteEnv,
		Mode:      "development",
		Prebundle: esmPrebundleEnabled(),
	})
	// Proxy unserved requests (API calls) to the app. The app's real bind
	// port is discovered from its startup log AFTER this server is already
	// listening, so resolve the target lazily: prefer the detected addr,
	// fall back to the flag default until it's known.
	srv := viteless.NewServer(host, viteless.WithProxyResolver(func() string {
		if v, ok := detectedAppAddr.Load().(string); ok && v != "" {
			return esmProxyTarget(v)
		}
		return esmProxyTarget(appAddr)
	}))

	ln, fellBack, err := esmListen()
	if err != nil {
		return fmt.Errorf("esm dev: bind: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	devURL := fmt.Sprintf("http://localhost:%d/", port)
	if fellBack {
		fmt.Fprintf(stdout, "%s●%s esm dev: port %s busy — using %d instead\n", ansiYellow, ansiReset, esmPreferredPort(), port)
	}

	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
		if closeVue != nil {
			closeVue()
		}
	}()
	go func() {
		if serveErr := httpSrv.Serve(ln); serveErr != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "%s●%s esm dev server: %v\n", ansiYellow, ansiReset, serveErr)
		}
	}()

	// File watcher → HMR broadcasts. Source edits map to their served URL;
	// the viteless client re-imports (self-accepting modules hot-swap, the
	// rest fall back to reload). Non-source edits (index.html, configs) →
	// full reload.
	go esmWatch(ctx, servedRoot, host, srv.HMR(), stdout)

	if frontendURLCh != nil {
		select {
		case frontendURLCh <- devURL:
		default:
		}
	}
	fmt.Fprintf(stdout, "%s●%s esm dev — unbundled native-ESM server on %s%s%s\n", ansiCyan, ansiReset, ansiBold, devURL, ansiReset)
	if verbose {
		fmt.Fprintf(stdout, "%s●%s root=%s  proxy→%s\n", ansiCyan, ansiReset, servedRoot, esmProxyTarget(appAddr))
	}
	return nil
}

// esmWatch fans filesystem changes under servedRoot into HMR updates.
func esmWatch(ctx context.Context, servedRoot string, host *devserver.Host, hub *viteless.HMR, stdout io.Writer) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return
	}
	defer w.Close()
	if err := hmrAddRecursive(w, servedRoot); err != nil {
		fmt.Fprintf(stdout, "%s[hmr]%s watch setup: %v\n", ansiYellow, ansiReset, err)
		return
	}
	debounce := map[string]time.Time{}
	const window = 80 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.Events:
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			now := time.Now()
			if last, ok := debounce[ev.Name]; ok && now.Sub(last) < window {
				continue
			}
			debounce[ev.Name] = now

			rel, rerr := filepath.Rel(servedRoot, ev.Name)
			if rerr != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			base := filepath.Base(ev.Name)
			urlPath := "/" + filepath.ToSlash(rel)
			switch {
			case strings.HasSuffix(base, ".css"), strings.HasSuffix(base, ".scss"), strings.HasSuffix(base, ".sass"):
				hub.Broadcast(viteless.Update{Type: "css", Path: urlPath})
			case strings.HasSuffix(base, ".vue"), strings.HasSuffix(base, ".ts"),
				strings.HasSuffix(base, ".tsx"), strings.HasSuffix(base, ".jsx"),
				strings.HasSuffix(base, ".js"), strings.HasSuffix(base, ".mjs"):
				// Self-accepting modules (.vue carry the HMR footer) hot-swap;
				// the viteless client falls back to a reload for the rest.
				hub.Broadcast(viteless.Update{Type: "update", Path: urlPath})
			case base == "index.html":
				// The shell changed — only a full reload picks that up.
				hub.Reload()
			default:
				// Editor temp/swap/backup files, .map, dotfiles, partial
				// atomic-rename artifacts, etc. IGNORE them. Reloading here
				// raced the real source update (the editor writes a sibling
				// temp on every save) and the reload won, so HMR never
				// applied. Only the file types above drive HMR.
			}
			_ = host
		case <-w.Errors:
		}
	}
}

// esmAliases returns the import aliases the dev server honors, parsed from
// the project's tsconfig.json "paths" so EVERY mapping the editor resolves
// (e.g. "nexus-client/vue" → src/sdk/vue.js, the generated GraphQL SDK)
// also resolves at runtime — not just the "@/" convention. Each paths entry
// becomes an alias:
//
//	"@/*":            ["./src/*"]          → wildcard  @/ → <base>/src
//	"nexus-client":   ["src/sdk/client.js"] → exact    nexus-client → that file
//	"nexus-client/vue":["src/sdk/vue.js"]   → exact
//
// Targets resolve against the tsconfig's baseUrl (default ".") which sits at
// servedRoot. Falls back to the "@/" → src convention when there's no
// tsconfig or no usable paths.
func esmAliases(servedRoot string) []devserver.Alias {
	def := func() []devserver.Alias {
		srcSub := filepath.Join(servedRoot, "src")
		if info, err := os.Stat(srcSub); err == nil && info.IsDir() {
			return []devserver.Alias{{Prefix: "@/", Dir: srcSub}}
		}
		return []devserver.Alias{{Prefix: "@/", Dir: servedRoot}}
	}

	paths, baseURL, ok := readTSConfigPaths(filepath.Join(servedRoot, "tsconfig.json"))
	if !ok {
		// also try jsconfig.json
		paths, baseURL, ok = readTSConfigPaths(filepath.Join(servedRoot, "jsconfig.json"))
	}
	if !ok || len(paths) == 0 {
		return def()
	}
	base := filepath.Join(servedRoot, filepath.FromSlash(baseURL))

	var aliases []devserver.Alias
	for key, targets := range paths {
		if len(targets) == 0 {
			continue
		}
		target := targets[0] // first candidate wins (TS tries in order)
		if strings.HasSuffix(key, "/*") && strings.HasSuffix(target, "/*") {
			// Wildcard: "@/*" → "./src/*" ⇒ prefix "@/" maps onto <base>/src.
			prefix := strings.TrimSuffix(key, "*") // keep trailing "/" → "@/"
			dir := filepath.Join(base, filepath.FromSlash(strings.TrimSuffix(target, "*")))
			aliases = append(aliases, devserver.Alias{Prefix: prefix, Dir: dir})
		} else if !strings.Contains(key, "*") {
			// Exact: "nexus-client/vue" → "src/sdk/vue.js".
			aliases = append(aliases, devserver.Alias{
				Prefix: key,
				Dir:    filepath.Join(base, filepath.FromSlash(target)),
				Exact:  true,
			})
		}
	}
	if len(aliases) == 0 {
		return def()
	}
	// Guarantee an "@/" fallback exists even if tsconfig omitted it.
	hasAt := false
	for _, a := range aliases {
		if a.Prefix == "@/" {
			hasAt = true
		}
	}
	if !hasAt {
		aliases = append(aliases, def()...)
	}
	return aliases
}

// readTSConfigPaths extracts compilerOptions.paths + baseUrl from a
// tsconfig/jsconfig. Tolerant of JSONC (// and /* */ comments + trailing
// commas) since TS allows them. baseUrl defaults to ".".
func readTSConfigPaths(file string) (paths map[string][]string, baseURL string, ok bool) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, "", false
	}
	var cfg struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(stripJSONC(raw), &cfg); err != nil {
		return nil, "", false
	}
	base := cfg.CompilerOptions.BaseURL
	if base == "" {
		base = "."
	}
	return cfg.CompilerOptions.Paths, base, true
}

// stripJSONC removes // line comments, /* */ block comments, and trailing
// commas so a tsconfig with JSONC niceties parses as plain JSON. String
// literals are preserved (so a "//" inside a value isn't mangled).
func stripJSONC(b []byte) []byte {
	var out []byte
	inStr, esc, line, block := false, false, false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case line:
			if c == '\n' {
				line = false
				out = append(out, c)
			}
		case block:
			if c == '*' && i+1 < len(b) && b[i+1] == '/' {
				block = false
				i++
			}
		case inStr:
			out = append(out, c)
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			line = true
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			block = true
			i++
		default:
			out = append(out, c)
		}
	}
	// Drop trailing commas: ",}" → "}", ",]" → "]" (whitespace-tolerant).
	return trailingCommaRE.ReplaceAll(out, []byte("$1"))
}

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// esmDefaultPort is the dev server's stable default — Vite's port, so it's
// instantly familiar and bookmarkable across runs. A fixed port also means
// the dev tab survives a `nexus dev` restart without changing the URL.
const esmDefaultPort = "5173"

// esmPreferredPort is the port the dev server tries first: NEXUS_DEV_ESM_ADDR
// (full host:port) wins; otherwise the stable default on loopback.
func esmPreferredPort() string {
	if v := os.Getenv("NEXUS_DEV_ESM_ADDR"); v != "" {
		return v
	}
	return esmDefaultPort
}

// esmListen binds the dev server. It tries the preferred (stable) port
// first; if that's already in use — a second project, a leftover process,
// or real Vite — it falls back to an OS-assigned port so `nexus dev` never
// fails to start over a port clash. Returns the listener, whether it fell
// back, and any error.
func esmListen() (net.Listener, bool, error) {
	pref := esmPreferredPort()
	addr := pref
	// A bare port ("5173") or env value without host → bind on loopback.
	if !strings.Contains(addr, ":") {
		addr = "127.0.0.1:" + addr
	} else if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	if ln, err := net.Listen("tcp", addr); err == nil {
		return ln, false, nil
	}
	// Preferred port busy → OS-assigned fallback.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, false, err
	}
	return ln, true, nil
}

// esmProxyTarget turns the app's bind address into a proxy origin the dev
// server forwards unserved requests (API calls) to.
func esmProxyTarget(appAddr string) string {
	return "http://" + normalizeProbeAddr(appAddr)
}

// esmDevEnabled reports whether the unbundled native-ESM dev server is used.
// It is now the DEFAULT for `nexus dev` (Vite-grade state-preserving HMR,
// zero Node); set NEXUS_DEV_ESM=0 (or false/off/no) to fall back to the
// legacy esbuild bundler watcher.
func esmDevEnabled() bool {
	return envBoolDefault("NEXUS_DEV_ESM", true)
}

// esmPrebundleEnabled reports whether dependency pre-bundling is on. On by
// DEFAULT alongside the ESM server — it collapses each npm package's
// intra-module fan-out into one file (Vuetify alone is ~1800 modules),
// cutting cold-load HTTP requests dramatically, with a cross-restart disk
// cache. Set NEXUS_DEV_PREBUNDLE=0 to disable; the per-module path remains
// the fallback for any package that fails to bundle.
func esmPrebundleEnabled() bool {
	return envBoolDefault("NEXUS_DEV_PREBUNDLE", true)
}

// envBoolDefault reads a boolean-ish env var with an explicit default. An
// unset/empty var yields def; "0"/"false"/"off"/"no" force false; any other
// non-empty value forces true. Case-insensitive.
func envBoolDefault(name string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	if v == "" {
		return def
	}
	switch v {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
