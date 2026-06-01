package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/paulmanoni/nexus/client"
	"github.com/spf13/cobra"
)

// findViteConfig returns the absolute path to the first
// vite.config.{ts,js,mts,mjs} sitting at the root of frontendDir,
// or "" when none exists. Probed in priority order so a project
// using TypeScript wins over the legacy `.js` shape if both
// happen to be present.
func findViteConfig(frontendDir string) string {
	for _, name := range []string{"vite.config.ts", "vite.config.mts", "vite.config.js", "vite.config.mjs"} {
		p := filepath.Join(frontendDir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// newDevCmd builds `nexus dev` — runs `go run` on the target package
// with a startup banner and auto-opens the dashboard once the configured
// port responds. Cobra wraps the runner.
func newDevCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		addr        string
		open        bool
		openDash    bool
		tui         bool
		noWatch     bool
		frontendDir string
		frontendCmd string
		verbose     bool
		bundleMode  bool
	)
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Run the app with go run + a live dashboard",
		Long: `Boot the user's app via 'go run', print a friendly banner, and
serve the dashboard once the listen port responds. Pass --open to also
launch a browser.

Use this instead of 'go run .' when you want one-command iteration. The
dev runner kills the entire process group on SIGINT/SIGTERM so the
compiled binary doesn't survive Ctrl-C as a zombie.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "."
			if len(args) > 0 {
				target = args[0]
			}
			if tui {
				return runDevTUI(target, addr, openDash, stdout, stderr)
			}
			return runDev(target, addr, open, openDash, !noWatch, frontendDir, frontendCmd, verbose, bundleMode, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultDevAddr,
		"dashboard address to probe and open")
	cmd.Flags().BoolVar(&open, "open", false,
		"launch a browser when the port responds (off by default)")
	cmd.Flags().BoolVar(&openDash, "open-dash", false,
		"open the /__nexus/ admin dashboard instead of the app's root URL")
	cmd.Flags().BoolVar(&tui, "tui", false,
		"interactive Bubble Tea UI: log pane + restart hotkey + ready indicator")
	cmd.Flags().BoolVar(&noWatch, "no-watch", false,
		"disable file-watch auto-rebuild (single-process mode only)")
	cmd.Flags().StringVar(&frontendDir, "frontend", "",
		"path to a frontend project (e.g. ./web); spawns its watcher alongside go run and prefixes its logs with [web]")
	cmd.Flags().StringVar(&frontendCmd, "frontend-cmd", "",
		"command run inside --frontend dir; default is `npm run dev` (vite dev server, HMR) or `npm run dev:build` with --bundle")
	cmd.Flags().BoolVar(&bundleMode, "bundle", false,
		"use vite build --watch + Go-served dist instead of vite dev server (slower, but produces an embeddable bundle continuously)")
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"keep [Fx] graph chatter, [GIN-debug] route-registration, and [web] frontend build output (all suppressed by default in dev)")
	return cmd
}

// defaultDevAddr is the --addr flag's default and the probe target
// when the user doesn't override it. We rely on the framework's
// "nexus: listening on …" output to discover the real bind, so the
// flag is mostly a fallback for non-nexus apps; users running plain
// nexus apps don't need to set it.
const defaultDevAddr = ":8080"

type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// runDev is the dev-loop body. Separated from the cobra wrapper so the
// happy path (start child → race signal vs natural exit → clean kill)
// reads top-to-bottom without being interleaved with flag parsing.
//
// When watch is true, runs a fsnotify watcher on the target dir and
// restarts `go run` on every coalesced source-file change. SIGINT
// stops the loop and tears down the active child cleanly.
func runDev(target, addr string, openOnReady, openDash, watch bool, frontendDir, frontendCmd string, verbose, bundleMode bool, stdout, stderr io.Writer) error {
	printDevBanner(stdout, target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Optional frontend watcher — runs alongside the Go process. Logs
	// stream into the same terminal under a [web] prefix so build
	// progress is visible in one place. Lifecycle is tied to ctx, so
	// SIGINT to nexus dev tears the watcher down too. Surviving
	// across Go restarts is the point: the frontend toolchain has
	// its own file watcher and shouldn't bounce on every Go save.
	//
	// Auto-detect: when --frontend isn't passed but the target package
	// declares nexus.ServeFrontend(distFS, "web/dist"), derive "web"
	// as the watcher dir. The user's "I want auto-rebuild" intent is
	// implicit in registering ServeFrontend, so requiring the flag is
	// just friction. The detection is best-effort — non-literal
	// embed roots fall through to the explicit-flag path.
	if frontendDir == "" {
		root, _ := os.Getwd()
		pkgDir := filepath.Join(root, target)
		if d := detectFrontendDir(pkgDir); d != "" {
			frontendDir = d
			if verbose {
				fmt.Fprintf(stdout, "%s●%s detected ServeFrontend → watching %s\n", ansiCyan, ansiReset, frontendDir)
			}
		}
	}

	// Single binary — no manifest-aware overlay. The vite proxy points
	// at the --addr flag's default :8080 unless the user overrides.
	overlayPath := ""
	proxyAddr := addr
	// frontendURLCh receives vite's "Local: http://..." URL when the
	// dev server (non-bundle mode) prints it. Buffered=1 so the
	// watcher's pump never blocks if no one's listening yet.
	frontendURLCh := make(chan string, 1)
	if frontendDir != "" {
		// Pick the right script + default command for the chosen
		// mode. Bundle mode keeps the legacy `vite build --watch`
		// path; the default mode runs vite's dev server with HMR.
		// A user-supplied --frontend-cmd overrides both.
		if frontendCmd == "" {
			if bundleMode {
				frontendCmd = "npm run dev:build"
			} else {
				frontendCmd = "npm run dev"
			}
		}
		// Inject the matching script into package.json when needed.
		// Idempotent against the named key, so projects that already
		// declare their own version keep it.
		if frontendCmd == "npm run dev:build" {
			if err := ensureDevBuildScript(frontendDir, stdout); err != nil {
				fmt.Fprintf(stderr, "package.json injection skipped: %v\n", err)
			}
		}
		if frontendCmd == "npm run dev" {
			if err := ensureDevServerScript(frontendDir, stdout); err != nil {
				fmt.Fprintf(stderr, "package.json injection skipped: %v\n", err)
			}
		}
		// The watch.exclude injection only matters for bundle mode —
		// the dev server doesn't re-bundle and doesn't loop on the
		// auto-import-plugin .d.ts regen.
		if bundleMode {
			if cfg := findViteConfig(frontendDir); cfg != "" {
				if err := client.EnsureViteWatchExclude(cfg, stdout); err != nil {
					fmt.Fprintf(stderr, "vite watch.exclude injection skipped: %v\n", err)
				}
			}
		}
		// Dev-server mode: the SPA fetches /__nexus/client/manifest.json
		// (and friends) at runtime. Without a vite proxy entry those
		// hit :8080 cross-origin from :5173 and CORS blocks them.
		// One-time injection alongside the existing /graphql /oauth
		// /ws rules. Bundle mode doesn't need this — same-origin
		// because Go serves the SPA itself.
		if !bundleMode {
			if cfg := findViteConfig(frontendDir); cfg != "" {
				apiURL := "http://localhost" + proxyAddr
				if !strings.HasPrefix(proxyAddr, ":") {
					apiURL = "http://" + proxyAddr
				}
				if err := client.EnsureViteProxyForNexus(cfg, apiURL, stdout); err != nil {
					fmt.Fprintf(stderr, "vite proxy injection skipped: %v\n", err)
				}
				// Auto-heal: re-sync the managed block whenever the
				// user edits vite.config.ts directly (e.g., deletes
				// the proxy block). Without this watcher, the sync
				// only fires on a Go restart and a manual config
				// edit would leave the SPA broken until the user
				// touches a .go file or restarts nexus dev.
				proxyURL := "http://localhost" + proxyAddr
				if !strings.HasPrefix(proxyAddr, ":") {
					proxyURL = "http://" + proxyAddr
				}
				go watchAndResyncViteProxy(ctx, proxyAddr, cfg, proxyURL, stdout, stderr)
			}
		}
		if err := startFrontendWatcher(ctx, frontendDir, addr, frontendCmd, verbose, stdout, stderr, frontendURLCh); err != nil {
			fmt.Fprintf(stderr, "frontend watcher disabled: %v\n", err)
			frontendURLCh = nil
		}
	} else {
		frontendURLCh = nil
	}

	var restartCh chan struct{}
	if watch {
		restartCh = make(chan struct{}, 1)
		root, _ := os.Getwd()
		// In dev mode the frontend dir is owned by the frontend
		// watcher (vite, esbuild, etc.); Go has no business
		// rebuilding when its files change. Override the embed-root
		// rule so saves under web/ don't bounce the Go process.
		ignore := []string{}
		if frontendDir != "" {
			abs, _ := filepath.Abs(frontendDir)
			if abs != "" {
				ignore = append(ignore, abs)
			}
		}
		if err := watchSource(ctx, root, restartCh, stderr, ignore); err != nil {
			fmt.Fprintf(stderr, "watcher disabled: %v\n", err)
			restartCh = nil
		}
	}

	// First boot announces the dashboard URL via waitAndOpen. Subsequent
	// restarts skip the open-browser branch (user already has the tab).
	first := true
	for {
		// Pass the frontend URL channel only on the first boot.
		// Subsequent Go restarts shouldn't re-open browsers, and the
		// vite dev server is already running anyway.
		viteURLForOpen := frontendURLCh
		if !first {
			viteURLForOpen = nil
		}
		exited, killChild, err := startDevChild(ctx, target, addr, overlayPath, openOnReady && first, openDash, verbose, viteURLForOpen, stdout, stderr)
		if err != nil {
			return err
		}
		// Auto-codegen for frontend.Plugin apps. Runs alongside the
		// boot-banner goroutine; both probe the same listen port
		// independently, and devCodegenWatch silently no-ops when no
		// --frontend dir was passed or no frontend.Plugin is
		// registered on the running app. Each iteration of the loop
		// (Go restart) re-fires the codegen so schema changes flow
		// into the TS tree without a manual `nexus generate frontend`.
		// proxyURL is what we ask the SPA's vite proxy to forward
		// to — manifest port wins (proxyAddr) over the --addr flag,
		// computed once above. Empty when no frontend so the codegen
		// path skips the sync.
		proxyURL := ""
		if frontendDir != "" && proxyAddr != "" {
			if strings.HasPrefix(proxyAddr, ":") {
				proxyURL = "http://localhost" + proxyAddr
			} else {
				proxyURL = "http://" + proxyAddr
			}
		}
		// Probe the manifest-derived bind addr (proxyAddr), not the
		// --addr flag's default. The user's manifest pins a port
		// the binary actually listens on; probing the flag's default
		// would time out and the post-boot sync (which would add
		// module RoutePrefix-derived entries to the vite proxy)
		// would silently never fire.
		go devCodegenWatch(ctx, proxyAddr, frontendDir, "vue", proxyURL, stdout, stderr)
		first = false
		select {
		case err := <-exited:
			if err != nil {
				// Compile error or panic. With the watcher running we
				// don't tear down the loop — the user fixes the bug,
				// the next save triggers a restart. Without it, exit
				// like the legacy single-shot path.
				if restartCh == nil {
					return fmt.Errorf("app exited: %w", err)
				}
				fmt.Fprintf(stderr, "%s●%s app exited: %v · waiting for changes\n", ansiYellow, ansiReset, err)
				select {
				case <-ctx.Done():
					return nil
				case <-restartCh:
					fmt.Fprintf(stdout, "%s●%s change detected · rebuilding\n", ansiCyan, ansiReset)
					continue
				}
			}
			if restartCh == nil {
				return nil
			}
			// Clean exit + watcher running: idle until the next change.
			select {
			case <-ctx.Done():
				return nil
			case <-restartCh:
				fmt.Fprintf(stdout, "%s●%s change detected · rebuilding\n", ansiCyan, ansiReset)
			}
		case <-restartCh:
			fmt.Fprintf(stdout, "%s●%s change detected · rebuilding\n", ansiCyan, ansiReset)
			killChild()
		case <-ctx.Done():
			killChild()
			return nil
		}
	}
}

// startDevChild starts one `go run target` invocation and returns
// channels the caller selects on:
//   - exited: receives the child's wait error (or nil on clean exit)
//   - killChild: tear-down hook that SIGTERMs the process group and
//     escalates to SIGKILL after 5s
//
// When overlayPath is non-empty, it's passed via `go run
// -overlay=...` so the manifest-derived deploy-init (port,
// listeners, topology) gets compiled into the binary.
//
// Carved out of runDev so the watcher loop's select can stay readable.
func startDevChild(ctx context.Context, target, addr, overlayPath string, openOnReady, openDash, verbose bool, frontendURLCh <-chan string, stdout, stderr io.Writer) (<-chan error, func(), error) {
	args := []string{"run"}
	if overlayPath != "" {
		args = append(args, "-overlay="+overlayPath)
	}
	args = append(args, target)
	cmd := exec.Command("go", args...)
	// Tee stdout/stderr through addrFinder so we can detect the
	// actual bind address from gin's "Listening and serving HTTP on
	// :PORT" line. The user's own Config.Addr trumps our --addr flag
	// — without this scan, the banner would point at the flag's
	// guess (default :8080) when the user wrote :8083.
	detectedCh := make(chan string, 1)
	cmd.Stdout = newAddrFinder(stdout, detectedCh)
	cmd.Stderr = newAddrFinder(stderr, detectedCh)
	cmd.Stdin = os.Stdin
	// Hand the child a NEXUS_DEV signal so ServeFrontend swaps its
	// embed.FS for os.DirFS — a watching frontend toolchain (vite
	// build --watch, esbuild --watch) can update web/dist/ without
	// forcing a Go recompile. NEXUS_DEV_ROOT pins the disk root to
	// the dev target so users running from a different CWD still
	// resolve correctly. NEXUS_VERBOSE flips the framework's
	// quiet-by-default policy off (keeps [Fx] + [GIN-debug] logs).
	//
	// NEXUS_PEER_DEV / NEXUS_CONFIG_DEV auto-unlock the dev gates
	// in extension/peer and extension/config — `nexus dev` is
	// literally the operator saying "I'm doing dev work," so the
	// plugins' dev-mode guards should follow. Production runs
	// don't go through `nexus dev`, so the guards still protect
	// real deployments.
	env := append(os.Environ(),
		"NEXUS_DEV=1",
		"NEXUS_DEV_ROOT="+target,
		"NEXUS_PEER_DEV=1",
		"NEXUS_CONFIG_DEV=1",
	)
	if verbose {
		env = append(env, "NEXUS_VERBOSE=1")
	}
	cmd.Env = env
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, func() {}, fmt.Errorf("failed to start `go run %s`: %w", target, err)
	}

	// waitAndOpen runs even when --no-open is set so the user still
	// gets the green "ready" line — only the browser launch is gated
	// on openOnReady. When the vite dev server is running, prefer
	// its URL (HMR-aware, the right tab to live in); fall back to
	// the gin/probe URL when bundle mode owns the frontend.
	go waitAndOpen(ctx, addr, openOnReady, openDash, stdout, detectedCh, frontendURLCh)

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	pid := cmd.Process.Pid
	killChild := func() {
		// SIGTERM first to give shutdown handlers (HTTP graceful close,
		// fx hooks) a chance, then SIGKILL after a short grace period.
		// Drain `exited` fully before returning so the caller never
		// has to read from it again — double-reading a buffered chan
		// of size 1 deadlocks (caused Ctrl-C to hang in v0.21.x).
		_ = killProcessGroup(pid, syscall.SIGTERM)
		select {
		case <-exited:
			return
		case <-time.After(5 * time.Second):
		}
		_ = killProcessGroup(pid, syscall.SIGKILL)
		<-exited
	}
	return exited, killChild, nil
}

// waitAndOpen produces the "ready" line and (optionally) opens the
// dashboard once the app is up. Two signals race:
//
//  1. The user's Config.Addr — captured by addrFinder from gin's
//     "Listening and serving HTTP on :PORT" log line. Authoritative.
//  2. A periodic probe of the --addr flag value. Fallback for apps
//     that don't print a recognizable listen line (custom routers,
//     fasthttp, etc.).
//
// If detection fires and the address differs from what the user passed
// as --addr, we surface a correction line — a misleading banner is
// the symptom that drove this code, so making the discrepancy
// visible is part of the fix.
func waitAndOpen(ctx context.Context, addr string, openBrowserOnReady, openDash bool, stdout io.Writer, detectedCh <-chan string, frontendURLCh <-chan string) {
	flagAddr := normalizeProbeAddr(addr)

	probeOnce := func(target string) bool {
		conn, err := net.DialTimeout("tcp", target, 200*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	probeFlagAddr := func() bool {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return false
			case <-ticker.C:
				if probeOnce(flagAddr) {
					return true
				}
			}
		}
		return false
	}

	flagDone := make(chan bool, 1)
	go func() { flagDone <- probeFlagAddr() }()

	// Vite URL wins when present — if the frontend is being served
	// by the dev server, that's where the user wants to land. Falls
	// through to the gin URL when frontendURLCh is nil (bundle mode
	// or no frontend).
	//
	// Race detail: the API and the Vite dev server come up within
	// milliseconds of each other. Whichever channel fires first wins
	// Go's select, so an unlucky scheduler gave the user the API URL
	// (port 8080) even though the SPA was about to be ready on its
	// own port. The fix is to always wait for BOTH signals (with a
	// short grace window after the first one lands) so the SPA URL
	// gets a fair chance to be the primary destination.
	const frontendGrace = 1500 * time.Millisecond
	var ready, viteURL string
	gotReady := func(detected string, ok bool) {
		if detected != "" {
			ready = detected
		} else if ok {
			ready = addr
		}
	}

	// First arrival.
	select {
	case <-ctx.Done():
		return
	case viteURL = <-frontendURLCh:
	case detected := <-detectedCh:
		gotReady(detected, true)
	case ok := <-flagDone:
		gotReady("", ok)
	}

	// Wait briefly for the still-pending signals so the Vite URL can
	// catch up when an API signal landed first. A bounded deadline
	// keeps this from stalling the banner when there's no frontend.
	deadline := time.After(frontendGrace)
	for viteURL == "" || ready == "" {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			// Out of grace; print whatever we've got. If neither side
			// reported, fall through to the original "no signal"
			// behavior of returning silently.
			if viteURL == "" && ready == "" {
				return
			}
			goto done
		case u := <-frontendURLCh:
			if viteURL == "" {
				viteURL = u
			}
		case detected := <-detectedCh:
			if ready == "" {
				gotReady(detected, true)
			}
		case ok := <-flagDone:
			if ready == "" {
				gotReady("", ok)
			}
		}
	}
done:

	// If the user passed an explicit --addr that doesn't match the
	// actual bind, surface the gap. Default --addr (":8080") is
	// suppressed — we never claimed it on the banner anyway, so
	// there's nothing to "correct" for the user.
	if ready != "" && addr != defaultDevAddr && normalizeProbeAddr(ready) != flagAddr {
		fmt.Fprintf(stdout, "\n  %s→ %sbound on %s%s%s %s(--addr was %s)%s\n",
			ansiDim, ansiReset, ansiBold, ready, ansiReset, ansiDim, addr, ansiReset)
	}

	var primaryURL string
	if viteURL != "" {
		primaryURL = strings.TrimRight(viteURL, "/") + "/"
	} else if ready != "" {
		primaryURL = clientURL(ready)
		if openDash {
			primaryURL = dashboardURL(ready)
		}
	}
	if primaryURL == "" {
		return
	}
	printReadyLine(stdout, primaryURL, openBrowserOnReady)
	if viteURL != "" && ready != "" {
		// In dev-server mode the user lives at vite's URL, but the
		// framework dashboard is a separate Vue bundle baked into
		// the Go binary. Going through vite's proxy adds edge
		// cases (SSE streaming, asset-path resolution); pointing
		// at Go's port directly skips the proxy entirely.
		fmt.Fprintf(stdout, "  %sAPI:        %s%s\n", ansiDim, clientURL(ready), ansiReset)
		fmt.Fprintf(stdout, "  %sDashboard:  %s%s\n", ansiDim, dashboardURL(ready), ansiReset)
	}
	if openBrowserOnReady {
		_ = openBrowser(primaryURL)
	}
}

// addrFinder wraps an io.Writer to scan child output line-by-line
// for gin's "Listening and serving HTTP on :PORT" message. On first
// match, sends the address (e.g. ":8083") on ch and stops scanning;
// every subsequent write passes through verbatim.
type addrFinder struct {
	w    io.Writer
	ch   chan<- string
	mu   sync.Mutex
	buf  []byte
	done atomic.Bool
}

func newAddrFinder(w io.Writer, ch chan<- string) *addrFinder {
	return &addrFinder{w: w, ch: ch}
}

func (a *addrFinder) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	if a.done.Load() {
		return n, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.buf = append(a.buf, p...)
	for {
		i := bytes.IndexByte(a.buf, '\n')
		if i < 0 {
			break
		}
		line := a.buf[:i]
		a.buf = a.buf[i+1:]
		if m := ginListenRE.FindSubmatch(line); m != nil {
			if !a.done.Swap(true) {
				// Publish the real bind addr for the ESM dev server's
				// proxy resolver — it's constructed before the app boots
				// and only learns the true port (e.g. :9590, not the
				// --addr flag default) from this line.
				detectedAppAddr.Store(string(m[1]))
				select {
				case a.ch <- string(m[1]):
				default:
				}
			}
			a.buf = nil
			break
		}
	}
	return n, err
}

// detectedAppAddr holds the application's real bind address once the
// addrFinder has parsed it from the child's "nexus: listening on …" line.
// The ESM dev server (started before the app boots) reads it lazily via
// its proxy resolver. Empty until discovered.
var detectedAppAddr atomic.Value // string

// ginListenRE matches the framework's own startup announcement plus
// gin's debug- and release-mode listening lines:
//
//	nexus: listening on http://:8080                ← framework (preferred)
//	nexus: listening on :8080                       ← framework (legacy)
//	[GIN-debug] Listening and serving HTTP on :8080 ← bare-gin user
//	[GIN] Listening and serving HTTPS on :443
//
// First match wins — the framework line lands earlier and reports
// the actual bound address even when the user passed :0. The
// optional scheme prefix on the framework line is stripped via the
// non-capturing group so the (\S+) we keep is always a bare
// host:port that clientURL / dashboardURL can prepend "http://" to.
var ginListenRE = regexp.MustCompile(`(?:nexus: listening on|Listening and serving (?:HTTP|HTTPS) on) (?:https?://)?(\S+)`)

// --- terminal styling ---
//
// We don't pull in a TUI library for the static banner — bubbletea
// would take over the whole terminal and conflict with the child's
// own stdout streaming. Plain ANSI escapes suffice; on non-tty
// stdout (`nexus dev | tee log`) the escapes appear as harmless
// noise around otherwise-readable text.

const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiCyan   = "\033[36m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiRed    = "\033[31m"
)

// nexusArt is the built-in NEXUS wordmark printed atop the dev banner.
// Figlet "standard" font; kept as raw-string lines so the backslashes
// in the glyphs survive verbatim. A project can replace it by dropping
// a banner.txt in the directory nexus dev runs from (see loadBannerArt).
var nexusArt = []string{
	` _   _ _______  ___   _ ____`,
	`| \ | | ____\ \/ / | | / ___|`,
	`|  \| |  _|  \  /| | | \___ \`,
	`| |\  | |___ /  \| |_| |___) |`,
	`|_| \_|_____/_/\_\\___/|____/`,
}

// loadBannerArt returns the wordmark lines to print atop the dev banner.
// When a banner.txt exists in dir (the directory nexus dev was invoked
// from), its contents replace the built-in NEXUS art verbatim — only a
// trailing newline is trimmed, so a project can ship its own wordmark
// or message and have it rendered exactly. Any read error or an empty
// file falls back to nexusArt.
func loadBannerArt(dir string) []string {
	b, err := os.ReadFile(filepath.Join(dir, "banner.txt"))
	if err != nil {
		return nexusArt
	}
	text := strings.TrimRight(string(b), "\n")
	if text == "" {
		return nexusArt
	}
	return strings.Split(text, "\n")
}

// printDevBanner writes the intro block that survives gin's debug
// firehose: the NEXUS wordmark (or a project's banner.txt override),
// a subtitle, and the target + starting rows. We deliberately omit the
// dashboard URL here: at this point we don't yet know what address the
// user's Config.Addr picked — printing a guess (the --addr flag's
// default) and "correcting" it later left a stale URL pinned at the top
// of the terminal even after the right one rendered below. The URL
// appears once, on the ready line, after the child binds.
func printDevBanner(w io.Writer, target string) {
	root, err := os.Getwd()
	if err != nil {
		root = "."
	}

	fmt.Fprintln(w)
	for _, line := range loadBannerArt(root) {
		fmt.Fprintf(w, "  %s%s%s%s\n", ansiBold, ansiCyan, line, ansiReset)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %sdev server · ctrl-c to stop%s\n\n", ansiDim, ansiReset)
	fmt.Fprintf(w, "  target     %s%s%s\n", ansiBold, target, ansiReset)
	fmt.Fprintf(w, "  %s%s starting…%s\n\n", ansiDim, ansiYellow+"●"+ansiDim, ansiReset)
}

// printReadyLine is the matching tail to the banner: a single green
// dot announces the port is live. Renders even when the browser
// auto-open is disabled so the user has an unambiguous "go ahead"
// signal in either mode.
func printReadyLine(w io.Writer, url string, openingBrowser bool) {
	if openingBrowser {
		fmt.Fprintf(w, "\n  %s●%s ready · %s%s%s %s· opening browser%s\n\n",
			ansiGreen, ansiReset, ansiCyan, url, ansiReset, ansiDim, ansiReset)
	} else {
		fmt.Fprintf(w, "\n  %s●%s ready · %s%s%s\n\n",
			ansiGreen, ansiReset, ansiCyan, url, ansiReset)
	}
}

// normalizeProbeAddr turns a listen spec into a dialable host:port.
// Empty hosts (":8080"), IPv6 wildcard ("[::]:8080"), and IPv4
// wildcard ("0.0.0.0:8080") all become "localhost:8080" so probes
// from within the dev runner connect to a loopback the OS actually
// routes.
func normalizeProbeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	if strings.HasPrefix(addr, "[::]:") {
		return "localhost:" + strings.TrimPrefix(addr, "[::]:")
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "localhost:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

// dashboardURL renders the full dashboard URL for the banner. Mirrors
// normalizeProbeAddr's localhost rewrite so the printed link is
// always clickable — `http://[::]:8080/...` would resolve as the
// IPv6 wildcard, which most terminal-based URL openers reject.
func dashboardURL(addr string) string {
	host := normalizeProbeAddr(addr)
	return "http://" + host + "/__nexus/"
}

// clientURL is dashboardURL's app-side counterpart: the root URL the
// user's own routes serve from. This is what auto-open targets by
// default — landing on the admin dashboard is opt-in via --open-dash.
func clientURL(addr string) string {
	host := normalizeProbeAddr(addr)
	return "http://" + host + "/"
}

// openBrowser dispatches to the platform's URL-opening tool. Errors are
// swallowed by the caller — missing `xdg-open` on a headless Linux box
// shouldn't fail the whole dev session.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{url}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default: // linux, freebsd, etc.
		name = "xdg-open"
		args = []string{url}
	}
	// #nosec G204 -- CLI helper, url is operator-supplied
	return exec.Command(name, args...).Start()
}
