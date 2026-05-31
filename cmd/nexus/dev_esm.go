package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	onDemand := makeOnDemandFetch(lf, st, lockPath, stdout)
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
		Root:     servedRoot,
		Resolver: resOpts,
		Compiler: compiler,
		Aliases:  esmAliases(servedRoot),
		Env:      viteEnv,
		Mode:     "development",
	})
	srv := viteless.NewServer(host, viteless.WithProxy(esmProxyTarget(appAddr)))

	ln, err := net.Listen("tcp", esmListenAddr())
	if err != nil {
		return fmt.Errorf("esm dev: bind: %w", err)
	}
	devURL := fmt.Sprintf("http://localhost:%d/", ln.Addr().(*net.TCPAddr).Port)

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
			urlPath := "/" + filepath.ToSlash(rel)
			switch strings.ToLower(filepath.Ext(ev.Name)) {
			case ".css", ".scss", ".sass":
				hub.Broadcast(viteless.Update{Type: "css", Path: urlPath})
			case ".vue", ".ts", ".tsx", ".jsx", ".js", ".mjs":
				// Self-accepting modules (.vue carry the HMR footer) hot-swap;
				// the viteless client falls back to a reload for the rest.
				hub.Broadcast(viteless.Update{Type: "update", Path: urlPath})
			default:
				// index.html, json config, anything else → full reload.
				hub.Reload()
			}
			_ = host
		case <-w.Errors:
		}
	}
}

// esmAliases returns the tsconfig-style path aliases the dev server honors.
// MVP covers the dominant Vite convention `@/* → <root>/src/*`; richer
// tsconfig "paths" parsing can layer on later.
func esmAliases(servedRoot string) []devserver.Alias {
	srcSub := filepath.Join(servedRoot, "src")
	if info, err := os.Stat(srcSub); err == nil && info.IsDir() {
		return []devserver.Alias{{Prefix: "@/", Dir: srcSub}}
	}
	return []devserver.Alias{{Prefix: "@/", Dir: servedRoot}}
}

// esmListenAddr is the address the dev server binds. NEXUS_DEV_ESM_ADDR
// overrides; default is an OS-assigned loopback port (":0").
func esmListenAddr() string {
	if v := os.Getenv("NEXUS_DEV_ESM_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:0"
}

// esmProxyTarget turns the app's bind address into a proxy origin the dev
// server forwards unserved requests (API calls) to.
func esmProxyTarget(appAddr string) string {
	return "http://" + normalizeProbeAddr(appAddr)
}

// esmDevEnabled reports whether the unbundled native-ESM dev server is
// opted into via NEXUS_DEV_ESM (any non-empty value except "0"/"false").
func esmDevEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("NEXUS_DEV_ESM")))
	return v != "" && v != "0" && v != "false"
}
