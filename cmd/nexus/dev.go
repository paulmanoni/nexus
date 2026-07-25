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

	toml "github.com/pelletier/go-toml/v2"
	"github.com/spf13/cobra"

	"github.com/paulmanoni/nexus"
	"github.com/paulmanoni/viteless"
)

// nexusTOMLPath returns the nexus.toml path for a dev target (a package dir
// or a file path).
func nexusTOMLPath(target string) string {
	dir := target
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		dir = filepath.Dir(target)
	}
	return filepath.Join(dir, "nexus.toml")
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
		fast        bool
		legacyGoRun bool
		distWatch   bool
		rawLogs     bool
		logFormat   string
		logPattern  string
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
		RunE: func(cmd *cobra.Command, args []string) error {
			target := "."
			if len(args) > 0 {
				target = args[0]
			}
			// When the user didn't pass --addr, take the app's real bind
			// address from nexus.toml ([runtime.server].addr). nexus dev
			// PROBES and PROXIES this address: the SPA's vite proxy must
			// target the port the binary actually listens on, not the
			// flag's :8080 default. Without this an app pinned to, say,
			// :9590 in nexus.toml gets a proxy pointed at :8080 and every
			// /graphql + module call from the SPA 404s.
			if !cmd.Flags().Changed("addr") {
				if a := devAddrFromConfig(target); a != "" {
					addr = a
				}
			}
			if tui {
				return runDevTUI(target, addr, openDash, stdout, stderr)
			}
			return runDev(target, addr, open, openDash, !noWatch, frontendDir, frontendCmd, verbose, fast, legacyGoRun, distWatch, rawLogs, logFormat, logPattern, stdout, stderr)
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
		"command run inside --frontend dir; default `npm run dev` (Vite dev server + HMR)")
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"keep [Fx] graph chatter, [GIN-debug] route-registration, and [web] frontend build output (all suppressed by default in dev)")
	cmd.Flags().BoolVar(&fast, "fast", false,
		"strip DWARF from the dev binary (-ldflags=-w) for faster per-restart linking; disables delve and trims panic stack detail")
	cmd.Flags().BoolVar(&legacyGoRun, "go-run", false,
		"legacy dev loop: launch via `go run`, killing the app before every rebuild (default: build-then-swap — the old binary keeps serving while the next one compiles)")
	cmd.Flags().BoolVar(&distWatch, "dist", false,
		"also keep web/dist rebuilt in the background (debounced viteless build) so go build / the production embed always matches the live frontend")
	cmd.Flags().BoolVar(&rawLogs, "raw-logs", false,
		"print the app's raw log lines instead of the columnar Dev Server Logs view (auto-disabled when stdout isn't a tty)")
	cmd.Flags().StringVar(&logFormat, "log-format", "",
		"dev log formatter: pretty (default) | logfmt | pattern | raw/json. Overrides [runtime.logging] format in nexus.toml")
	cmd.Flags().StringVar(&logPattern, "log-pattern", "",
		"custom layout when --log-format=pattern, e.g. \"%time %-5level %caller %msg %fields\" (Spring-style tokens). Overrides [runtime.logging] pattern")
	return cmd
}

// defaultDevAddr is the --addr flag's default and the probe target
// when the user doesn't override it. We rely on the framework's
// "nexus: listening on …" output to discover the real bind, so the
// flag is mostly a fallback for non-nexus apps; users running plain
// nexus apps don't need to set it.
const defaultDevAddr = ":8080"

// devAddrFromConfig reads [runtime.server].addr from nexus.toml in the
// dev target's directory, so `nexus dev` probes + proxies the address
// the app actually binds. Returns "" when the file, the table, or the
// key is absent (or unparsable) — the caller then keeps the --addr
// flag. Deliberately decodes ONLY runtime.server.addr: a partial,
// lenient parse that never fails the dev loop over an unrelated config
// quirk, and stays independent of the framework's full config schema.
func devAddrFromConfig(target string) string {
	dir := target
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		dir = filepath.Dir(target)
	}
	b, err := os.ReadFile(filepath.Join(dir, "nexus.toml"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Runtime struct {
			Server struct {
				Addr string `toml:"addr"`
			} `toml:"server"`
		} `toml:"runtime"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return ""
	}
	return strings.TrimSpace(cfg.Runtime.Server.Addr)
}

// inertiaImportPath is the inertia extension package. An app that imports it is
// (almost certainly) an Inertia app, which is the auto-detect signal below.
const inertiaImportPath = "github.com/paulmanoni/nexus/extension/inertia"

// devInertiaEnabled reports whether `nexus dev` should use the Inertia dev
// topology. Inertia inverts the normal dev model: pages are server-rendered by
// the Go app, so the browser must live at the app's port (not viteless's), and
// the app's document shell references the viteless dev server for HMR/assets.
//
// Resolution: an explicit `[runtime.inertia] enabled` in nexus.toml wins
// (true forces it on, false forces it off — the override for hybrid apps). When
// the key is unset, it's auto-detected: an app whose build graph imports the
// inertia extension gets the Inertia topology with no config. When off,
// `nexus dev` behaves exactly as before (browser at the viteless SPA URL,
// viteless proxying to the app).
func devInertiaEnabled(target string) bool {
	if v, ok := inertiaConfigOverride(target); ok {
		return v
	}
	return appImportsPackage(target, inertiaImportPath)
}

// inertiaConfigOverride returns the explicit [runtime.inertia] enabled value and
// whether it was set at all (a *bool distinguishes unset from false, so an
// absent key falls through to auto-detection rather than forcing it off).
func inertiaConfigOverride(target string) (value, set bool) {
	b, err := os.ReadFile(filepath.Join(targetDir(target), "nexus.toml"))
	if err != nil {
		return false, false
	}
	var cfg struct {
		Runtime struct {
			Inertia struct {
				Enabled *bool `toml:"enabled"`
			} `toml:"inertia"`
		} `toml:"runtime"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return false, false
	}
	if cfg.Runtime.Inertia.Enabled == nil {
		return false, false
	}
	return *cfg.Runtime.Inertia.Enabled, true
}

// appImportsPackage reports whether importPath is in the build-graph closure of
// the main package at target (`go list -deps`). Used to auto-detect Inertia.
// Any error (no Go files, build broken) is treated as "not imported".
func appImportsPackage(target, importPath string) bool {
	cmd := exec.Command("go", "list", "-deps", ".")
	cmd.Dir = targetDir(target)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == importPath {
			return true
		}
	}
	return false
}

// targetDir returns the directory for a dev target (a package dir, or the parent
// of a target that points at a single file). Empty/unknown -> the cwd ".".
func targetDir(target string) string {
	if target == "" {
		return "."
	}
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		return filepath.Dir(target)
	}
	return target
}

// devLoggingFromConfig reads [runtime.logging] from nexus.toml in the dev
// target's dir — the Django-/Spring-style declarative log config. Returns the
// formatter name and (for format="pattern") the layout string; both empty when
// the file/table/keys are absent. Lenient like devAddrFromConfig: a parse
// error never fails the dev loop, it just falls back to the pretty default.
//
//	[runtime.logging]
//	format  = "pretty"   # pretty | logfmt | pattern | raw
//	pattern = "%time  %-5level  %caller  %msg  %fields"
func devLoggingFromConfig(target string) (format, pattern string) {
	dir := target
	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		dir = filepath.Dir(target)
	}
	b, err := os.ReadFile(filepath.Join(dir, "nexus.toml"))
	if err != nil {
		return "", ""
	}
	var cfg struct {
		Runtime struct {
			Logging struct {
				Format  string `toml:"format"`
				Pattern string `toml:"pattern"`
			} `toml:"logging"`
		} `toml:"runtime"`
	}
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return "", ""
	}
	return strings.TrimSpace(cfg.Runtime.Logging.Format), cfg.Runtime.Logging.Pattern
}

type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// runDev is the dev-loop body. Separated from the cobra wrapper so the
// happy path (start child → race signal vs natural exit → clean kill)
// reads top-to-bottom without being interleaved with flag parsing.
//
// When watch is true, runs a fsnotify watcher on the target dir and
// rebuilds on every coalesced source-file change. SIGINT stops the loop
// and tears down the active child cleanly.
//
// Rebuilds are build-then-swap: the next binary compiles while the
// current one keeps serving, and the swap happens only once the build
// is green (see devBuilder). legacyGoRun restores the old `go run`
// loop, which kills the app first and leaves it down for the whole
// compile.
func runDev(target, addr string, openOnReady, openDash, watch bool, frontendDir, frontendCmd string, verbose, fast, legacyGoRun, distWatch, rawLogs bool, logFormat, logPattern string, stdout, stderr io.Writer) error {
	printDevBanner(stdout, target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Columnar "Dev Server Logs" view: reshape the child's zap-JSON log lines
	// into the time · level · source · message layout. Off when --raw-logs is
	// set or stdout isn't a tty (so piping/redirecting keeps the raw JSON for
	// grep/jq). Color further honors NO_COLOR.
	//
	// Formatter selection (Django-/Spring-style): nexus.toml [runtime.logging]
	// sets the default { format, pattern }; the --log-format / --log-pattern
	// flags override per-run. format=raw|json|off bypasses the prettifier
	// entirely (raw JSON passes through).
	cfgFormat, cfgPattern := devLoggingFromConfig(target)
	if logFormat == "" {
		logFormat = cfgFormat
	}
	if logPattern == "" {
		logPattern = cfgPattern
	}
	prettyLogs := !rawLogs && stdoutIsTerminal()
	switch strings.ToLower(strings.TrimSpace(logFormat)) {
	case "raw", "json", "off", "none":
		prettyLogs = false
	}
	var logFmt logFormatter
	if prettyLogs {
		f, ok := resolveLogFormatter(logFormat, logPattern)
		if !ok {
			fmt.Fprintf(stderr, "%s●%s unknown --log-format %q · using pretty\n", ansiYellow, ansiReset, logFormat)
		}
		logFmt = f
	}

	// Inertia dev topology (auto-detected from the inertia import, or forced
	// via [runtime.inertia] enabled in nexus.toml). When on, the Go app owns
	// page navigation, so the browser opens the app port and the app shell
	// references viteless for HMR (NEXUS_VITE_DEV).
	//
	// Auto-detection shells out to `go list -deps` (~300ms, more on a large
	// module), and nothing before the child's launch depends on the answer —
	// so it runs off the critical path and the first compile starts without
	// waiting for it.
	inertiaFut := newFuture(func() bool { return devInertiaEnabled(target) })

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

	// overlayPath injects the decorator-form handler registrations
	// (//@rest / //@provide / …) for `nexus dev` WITHOUT writing any
	// nexus_handlers_gen.go into the source tree — zero churn while iterating.
	// It's regenerated at the top of each restart so annotation edits flow in
	// on the next reload; cleanupOverlay removes the previous temp dir.
	overlayPath := ""
	var cleanupOverlay func()
	defer func() {
		if cleanupOverlay != nil {
			cleanupOverlay()
		}
	}()
	// proxyAddr is the app's bind address used by the post-boot TS codegen
	// to fetch the running manifest (manifest port from --addr).
	proxyAddr := addr
	// frontendURLCh receives the dev server's "Local: http://..." URL when the
	// dev server (non-bundle mode) prints it. Buffered=1 so the
	// watcher's pump never blocks if no one's listening yet.
	frontendURLCh := make(chan string, 1)
	// frontendBase resolves to the dev server's base URL (empty when it
	// failed or none was started). Only the Inertia path has to wait for it,
	// so the dev server boots — deps, transforms and all — while the Go
	// build already runs.
	var frontendBase *future[string]
	if frontendDir != "" {
		// The frontend is served by the embedded viteless engine (Vite for
		// Go): a zero-Node HMR dev server that proxies unmatched requests
		// (/__nexus, /graphql, /oauth, /ws, API calls) back to the Go app.
		// If the project has a real Vite installed, viteless delegates to it;
		// otherwise it uses its own engine — no npm, no managed vite.config
		// proxy block. The Go app's real port is discovered from its startup
		// log, so the proxy target is resolved lazily.
		_ = frontendCmd // retained for flag compatibility; viteless owns the dev server
		proxyResolver := func() string {
			if v, ok := detectedAppAddr.Load().(string); ok && v != "" {
				return "http://" + normalizeProbeAddr(v)
			}
			a := addr
			if strings.HasPrefix(a, ":") {
				a = "127.0.0.1" + a
			}
			return "http://" + a
		}
		// Expose the nexus.toml [env] table to the frontend as
		// import.meta.env.<dotted.name> (e.g. import.meta.env.client.id).
		env, _ := nexus.EnvVars(nexusTOMLPath(target))
		frontendBase = newFuture(func() string {
			d, err := viteless.Dev(viteless.DevConfig{
				Root:          frontendDir,
				ProxyResolver: proxyResolver,
				Mode:          "development",
				Env:           env,
				Logf: func(format string, args ...any) {
					msg := fmt.Sprintf(format, args...)
					// Inertia dev is browser-at-the-app-port: the Vite server is
					// just an asset/HMR origin the user never visits, so keep its
					// routine chatter quiet and surface only errors.
					if inertiaFut.get() && !strings.Contains(strings.ToLower(msg), "error") {
						return
					}
					fmt.Fprintf(stdout, "%s[web]%s %s\n", ansiCyan, ansiReset, msg)
				},
			})
			if err != nil {
				fmt.Fprintf(stderr, "frontend dev server disabled: %v\n", err)
				return ""
			}
			go func() { <-ctx.Done(); d.Close() }()
			select {
			case frontendURLCh <- d.URL():
			default:
			}
			return d.URL()
		})
	} else {
		frontendURLCh = nil
	}

	// projectRoot is what the watchers walk and what .nexusignore patterns
	// are relative to: the directory nexus dev was invoked from.
	projectRoot, _ := os.Getwd()
	userIgnore := loadNexusIgnore(projectRoot)
	if userIgnore != nil {
		fmt.Fprintf(stdout, "  %s● %s · %d pattern(s)%s\n", ansiDim, nexusIgnoreFile, userIgnore.patterns(), ansiReset)
	}

	// --dist: mirror the live frontend into web/dist in the background so a
	// `go build` taken mid-session (or the production embed) always matches
	// the current source. Opt-in — it runs a full viteless build alongside
	// the HMR server. Independent of the dev server starting, so it works
	// even when viteless.Dev failed to come up.
	if distWatch && frontendDir != "" {
		env, _ := nexus.EnvVars(nexusTOMLPath(target))
		if err := watchDistBuild(ctx, frontendDir, env, userIgnore, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "%s●%s dist watch disabled: %v\n", ansiYellow, ansiReset, err)
		}
	}

	var restartCh chan struct{}
	if watch {
		restartCh = make(chan struct{}, 1)
		root := projectRoot
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

	// The compiler for the build-then-swap path. Nil in legacy --go-run
	// mode, where `go run` still owns compilation.
	var builder *devBuilder
	if !legacyGoRun {
		b, err := newDevBuilder(fast)
		if err != nil {
			return fmt.Errorf("dev build dir: %w", err)
		}
		builder = b
		defer builder.close()
	}

	// First boot announces the dashboard URL via waitAndOpen. Subsequent
	// restarts skip the open-browser branch (user already has the tab).
	first := true

	// Child + build state carried across loop iterations. lastHash is the
	// running binary's content hash, so a rebuild that produces identical
	// bytes can skip the swap entirely; prevBin is the superseded binary,
	// removed once its process is gone.
	var (
		exited    <-chan error
		killChild func()
		running   bool
		lastHash  string
		prevBin   string
		exitErr   error
	)
	defer func() {
		if killChild != nil {
			killChild()
		}
	}()

	// waitForChange parks until the watcher reports a source change,
	// returning false when the dev loop should end (SIGINT, or a child
	// that exited with no watcher to revive it). A child that dies while
	// we're parked is reported and we keep waiting — the next save
	// rebuilds and respawns it, which is why a crash-on-boot no longer
	// needs its own branch in the loop below.
	waitForChange := func() bool {
		for {
			var childExit <-chan error
			if running {
				childExit = exited
			}
			select {
			case <-ctx.Done():
				return false
			case err := <-childExit:
				running, killChild = false, nil
				if err != nil {
					if restartCh == nil {
						exitErr = fmt.Errorf("app exited: %w", err)
						return false
					}
					fmt.Fprintf(stderr, "%s●%s app exited: %v · waiting for changes\n", ansiYellow, ansiReset, err)
					continue
				}
				if restartCh == nil {
					return false
				}
			case <-restartCh:
				fmt.Fprintf(stdout, "%s●%s change detected · rebuilding\n", ansiCyan, ansiReset)
				return true
			}
		}
	}

	for {
		// Regenerate the handler-registration overlay from the current //@
		// annotations before each (re)launch, replacing the previous temp dir.
		// A scan error here usually means a source file won't compile either,
		// so the build below surfaces the real error — we just warn and drop
		// the overlay.
		if cleanupOverlay != nil {
			cleanupOverlay()
			cleanupOverlay = nil
		}
		if op, cl, err := buildHandlerOverlay(target); err != nil {
			fmt.Fprintf(stderr, "%s●%s handler codegen skipped: %v\n", ansiYellow, ansiReset, err)
			overlayPath = ""
		} else {
			overlayPath, cleanupOverlay = op, cl
		}

		// Build-then-swap. The child from the previous iteration is still
		// serving here — nothing is torn down until the build is green.
		binPath := ""
		if builder != nil {
			start := time.Now()
			bin, buildErr := builder.build(ctx, target, overlayPath, stderr)
			if ctx.Err() != nil {
				return exitErr
			}
			if buildErr != nil {
				// Compile error. With a watcher up, the running app (if
				// any) stays up and the user fixes the code; without one,
				// there's nothing to wait for.
				if restartCh == nil {
					return fmt.Errorf("build failed: %w", buildErr)
				}
				if running {
					fmt.Fprintf(stderr, "%s●%s build failed · still serving the previous build\n", ansiYellow, ansiReset)
				} else {
					fmt.Fprintf(stderr, "%s●%s build failed · waiting for changes\n", ansiYellow, ansiReset)
				}
				if !waitForChange() {
					return exitErr
				}
				continue
			}
			fmt.Fprintf(stdout, "  %s● built in %s%s\n", ansiDim, time.Since(start).Round(time.Millisecond), ansiReset)

			// Identical bytes mean the running process already IS this
			// build — the save didn't reach the app's build graph (a
			// _test.go edit, an unchanged buffer, another package's
			// files). Restarting would only cost the user their app state.
			h, herr := fileHash(bin)
			if running && herr == nil && h == lastHash {
				_ = os.Remove(bin)
				fmt.Fprintf(stdout, "  %s● binary unchanged · kept the running process%s\n", ansiDim, ansiReset)
				if !waitForChange() {
					return exitErr
				}
				continue
			}
			if prevBin != "" {
				_ = os.Remove(prevBin)
			}
			binPath, prevBin, lastHash = bin, bin, h

			// Pay the OS's first-exec cost (code-signature validation)
			// now, while the outgoing child is still answering requests.
			builder.prewarm(ctx, binPath)
		}

		// The port is single-occupancy, so the outgoing child dies only
		// now — after its replacement has compiled successfully.
		if killChild != nil {
			killChild()
			killChild, running = nil, false
		}

		// Pass the frontend URL channel only on the first boot.
		// Subsequent Go restarts shouldn't re-open browsers, and the
		// vite dev server is already running anyway.
		viteURLForOpen := frontendURLCh
		if !first {
			viteURLForOpen = nil
		}
		// In Inertia mode, hand the child the viteless dev URL (every
		// restart) so the app's document shell can reference it for HMR.
		// This is the one place that has to wait for the detection and the
		// dev server — by now the build has already run.
		inertiaViteURL := ""
		if inertiaFut.get() {
			if first {
				if _, set := inertiaConfigOverride(target); !set {
					fmt.Fprintf(stdout, "%s●%s Inertia app detected — serving pages at the app port (set [runtime.inertia] enabled = false to opt out)\n", ansiCyan, ansiReset)
				}
			}
			if frontendBase != nil {
				inertiaViteURL = frontendBase.get()
			}
		}
		ex, kill, err := startDevChild(ctx, binPath, target, addr, overlayPath, openOnReady && first, openDash, verbose, fast, prettyLogs, logFmt, viteURLForOpen, inertiaViteURL, stdout, stderr)
		if err != nil {
			return err
		}
		exited, killChild, running = ex, kill, true
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
		if !waitForChange() {
			return exitErr
		}
	}
}

// startDevChild launches one run of the app and returns channels the
// caller selects on:
//   - exited: receives the child's wait error (or nil on clean exit)
//   - killChild: tear-down hook that SIGTERMs the process group and
//     escalates to SIGKILL after 5s
//
// binPath names a binary devBuilder already compiled — the default
// build-then-swap path, which execs it directly (no resident `go run`
// supervisor, and the compile happened while the previous child was
// still serving). When binPath is empty (--go-run) we fall back to
// `go run`, which compiles here and so keeps the app down for the
// duration; overlayPath is then passed as `go run -overlay=...` so the
// decorator-form registrations still reach the build.
//
// The child inherits the CLI's working directory in both modes, so
// nexus.Boot resolves nexus.toml from the same place either way.
//
// Carved out of runDev so the watcher loop's select can stay readable.
func startDevChild(ctx context.Context, binPath, target, addr, overlayPath string, openOnReady, openDash, verbose, fast, prettyLogs bool, logFmt logFormatter, frontendURLCh <-chan string, inertiaViteURL string, stdout, stderr io.Writer) (<-chan error, func(), error) {
	cmd := exec.Command(binPath)
	if binPath == "" {
		// Legacy --go-run path. The flags mirror devBuilder.build:
		// -gcflags=all=-N -l skips the optimizer for the whole graph
		// (markedly faster compiles, and dev binaries are never
		// perf-sensitive), while --fast additionally strips DWARF so the
		// linker — the step no cache makes incremental — emits less. The
		// tradeoff there is that delve can't attach and panic traces lose
		// detail, which is why it stays opt-in.
		args := []string{"run"}
		if overlayPath != "" {
			args = append(args, "-overlay="+overlayPath)
		}
		args = append(args, "-gcflags=all=-N -l")
		if fast {
			args = append(args, "-ldflags=-w")
		}
		args = append(args, target)
		cmd = exec.Command("go", args...)
	}
	// Tee stdout/stderr through addrFinder so we can detect the
	// actual bind address from gin's "Listening and serving HTTP on
	// :PORT" line. The user's own Config.Addr trumps our --addr flag
	// — without this scan, the banner would point at the flag's
	// guess (default :8080) when the user wrote :8083.
	detectedCh := make(chan string, 1)
	// Pretty path: the addrFinder must still see RAW child bytes to detect the
	// "nexus: listening on …" line, so it wraps the prettifier (raw in →
	// detect → reshape → terminal), not the other way round.
	outW, errW := stdout, stderr
	if prettyLogs {
		color := os.Getenv("NO_COLOR") == ""
		outW = newLogPretty(stdout, color, logFmt)
		errW = newLogPretty(stderr, color, logFmt)
	}
	cmd.Stdout = newAddrFinder(outW, detectedCh)
	cmd.Stderr = newAddrFinder(errW, detectedCh)
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
	// Inertia: tell the app where the viteless dev server lives so its
	// document shell can load the HMR client + entry from there.
	if inertiaViteURL != "" {
		env = append(env, "NEXUS_VITE_DEV="+inertiaViteURL)
	}
	cmd.Env = env
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, func() {}, fmt.Errorf("failed to start %s: %w", strings.Join(cmd.Args, " "), err)
	}

	// waitAndOpen runs even when --no-open is set so the user still
	// gets the green "ready" line — only the browser launch is gated
	// on openOnReady. When the vite dev server is running, prefer
	// its URL (HMR-aware, the right tab to live in); fall back to
	// the gin/probe URL when bundle mode owns the frontend.
	go waitAndOpen(ctx, addr, openOnReady, openDash, inertiaViteURL != "", stdout, detectedCh, frontendURLCh)

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
func waitAndOpen(ctx context.Context, addr string, openBrowserOnReady, openDash, inertia bool, stdout io.Writer, detectedCh <-chan string, frontendURLCh <-chan string) {
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
	//
	// With no dev server in play (frontendURLCh nil) there is no second
	// signal to wait for, and burning the grace window would just delay
	// the ready line on every pure-Go app.
	deadline := time.After(frontendGrace)
	for (frontendURLCh != nil && viteURL == "") || ready == "" {
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
	switch {
	case inertia && ready != "":
		// Inertia: the app serves pages, so the browser lives at the app
		// port; viteless is only the HMR/asset origin the shell references.
		primaryURL = clientURL(ready)
		if openDash {
			primaryURL = dashboardURL(ready)
		}
	case viteURL != "":
		primaryURL = strings.TrimRight(viteURL, "/") + "/"
	case ready != "":
		primaryURL = clientURL(ready)
		if openDash {
			primaryURL = dashboardURL(ready)
		}
	}
	if primaryURL == "" {
		return
	}
	printReadyLine(stdout, primaryURL, openBrowserOnReady)
	if inertia {
		// Inertia: the Vite server is an internal asset/HMR origin the user
		// never opens — don't advertise its port.
	} else if viteURL != "" && ready != "" {
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
