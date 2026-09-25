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
		frontendCmd string // deprecated, ignored
		verbose     bool
		fast        bool
		debugBuild  bool
		noEmbedStub bool
		legacyGoRun bool
		distWatch   bool
		rawLogs     bool
		logFormat   string
		logPattern  string
	)
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Rebuild and rerun the app on every save",
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
			// address from nexus.toml ([runtime.server].addr): it is what
			// nexus dev probes for the ready line when the app prints no
			// "listening on" line of its own.
			if !cmd.Flags().Changed("addr") {
				if a := devAddrFromConfig(target); a != "" {
					addr = a
				}
			}
			// --debug is the inverse of --fast, and wins: it's the explicit
			// "I'm about to attach a debugger" signal, whereas --fast is now
			// just the default that happens to be spelled out.
			if debugBuild {
				fast = false
			}
			if tui {
				return runDevTUI(target, addr, openDash, frontendDir, verbose, stdout, stderr)
			}
			return runDev(target, addr, open, openDash, !noWatch, frontendDir, verbose, fast, !noEmbedStub, legacyGoRun, distWatch, rawLogs, logFormat, logPattern, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultDevAddr,
		"address the app listens on — what dev probes and opens (read from nexus.toml when not set)")
	cmd.Flags().BoolVar(&open, "open", false,
		"launch a browser when the port responds (off by default)")
	cmd.Flags().BoolVar(&openDash, "open-dash", false,
		"open the /__nexus/ admin dashboard instead of the app's root URL")
	cmd.Flags().BoolVar(&tui, "tui", false,
		"interactive Bubble Tea UI: log pane + restart hotkey + ready indicator")
	cmd.Flags().BoolVar(&noWatch, "no-watch", false,
		"disable file-watch auto-rebuild (single-process mode only)")
	cmd.Flags().StringVar(&frontendDir, "frontend", "",
		"frontend project dir (default: found from the app's ServeFrontend call, or NEXUS_FRONTEND_DIR); with a package.json, its Vite runs alongside the app, logging under [web]")
	cmd.Flags().StringVar(&frontendCmd, "frontend-cmd", "", "ignored")
	_ = cmd.Flags().MarkDeprecated("frontend-cmd", "it is ignored: nexus dev runs the frontend's own Vite (node_modules/.bin/vite)")
	cmd.Flags().BoolVar(&verbose, "verbose", false,
		"keep [Fx] graph chatter, [GIN-debug] route-registration, and Vite's full startup banner (all suppressed by default in dev)")
	cmd.Flags().BoolVar(&fast, "fast", true,
		"strip DWARF from the dev binary (-ldflags=-w) for faster per-restart linking")
	// On by default, so naming it does nothing; --debug is the reachable
	// inverse and the one worth documenting.
	_ = cmd.Flags().MarkDeprecated("fast", "it is the default; pass --debug to keep DWARF instead")
	cmd.Flags().BoolVar(&debugBuild, "debug", false,
		"keep DWARF in the dev binary so delve can attach and panic traces stay complete (slower link; the inverse of --fast)")
	cmd.Flags().BoolVar(&noEmbedStub, "no-embed-stub", false,
		"embed the real frontend bundle in the dev binary instead of stubbing it out (dev serves the bundle from disk, so the embedded copy is normally dead weight)")
	cmd.Flags().BoolVar(&legacyGoRun, "go-run", false,
		"legacy dev loop: launch via `go run`, killing the app before every rebuild (default: build-then-swap — the old binary keeps serving while the next one compiles)")
	cmd.Flags().BoolVar(&distWatch, "dist", false,
		"also keep web/dist rebuilt in the background (debounced `vite build`) so go build / the production embed always matches the live frontend")
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

// devKillGrace is how long the dev loop waits after SIGTERM before SIGKILLing
// the app — on Ctrl-C and on every build-then-swap restart. It only has to
// cover a healthy app's shutdown, which nexus bounds at DevShutdownTimeout;
// anything slower is wedged and waiting longer just makes Ctrl-C feel broken.
const devKillGrace = 750 * time.Millisecond

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
func runDev(target, addr string, openOnReady, openDash, watch bool, frontendDir string, verbose, fast, embedStub, legacyGoRun, distWatch, rawLogs bool, logFormat, logPattern string, stdout, stderr io.Writer) error {
	printDevBanner(stdout, target)

	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
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

	// The package directory, absolute: every path the source names (the
	// ServeFrontend root, a frontend.Plugin Root) is relative to it, not to
	// wherever nexus dev was started — `nexus dev ./examples/app` from the
	// repo root must find ./examples/app/web, not ./web.
	pkgDir, err := filepath.Abs(targetDir(target))
	if err != nil {
		return fmt.Errorf("resolve %s: %w", target, err)
	}

	// distStubRoot names the //go:embed tree the dev build replaces with
	// stubs — the bundle ServeFrontend mounts, which under NEXUS_DEV is read
	// from disk anyway (and whose pages load their modules from Vite while
	// it runs). Relinking it on every save is pure cost; see
	// distStubReplacements. Relative to the package, like the source says.
	//
	// Resolved once: the embed root is a string literal in the user's source,
	// so it can't change without a rebuild of the file that declares it.
	serveRoot := detectServeFrontendRoot(pkgDir)
	servedDist := ""
	if serveRoot != "" {
		servedDist = filepath.Join(pkgDir, serveRoot)
	}
	distStubRoot := ""
	if embedStub && serveRoot != "" {
		distStubRoot = servedDist
		if verbose {
			fmt.Fprintf(stdout, "%s●%s stubbing %s out of the dev build (served from disk; --no-embed-stub to embed it)\n", ansiCyan, ansiReset, serveRoot)
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

	// The frontend project (--frontend, NEXUS_FRONTEND_DIR, or the
	// ServeFrontend / frontend.Plugin call in the source) and, when it has
	// a package.json, its own Vite. Vite lives for the whole session — Go
	// rebuilds don't bounce it; it has its own watcher — and the app finds
	// it through the hot file, so nothing about it is passed to the child.
	// The deferred stop waits for Vite to exit, which is what lets its
	// plugin remove the hot file before nexus dev returns. A directory
	// without a package.json (a hand-written bundle) gets no dev server,
	// only the codegen and watcher treatment below.
	front := startDevFrontend(ctx, pkgDir, frontendDir, servedDist, devViteConfig{
		TOMLPath: nexusTOMLPath(target),
		Verbose:  verbose,
		Out:      stdout,
		Notes:    stderr,
		Color:    stdoutIsTerminal(),
	})
	frontendDir = front.Dir
	fp, vite := front.Project, front.Vite
	if vite != nil {
		defer vite.stop()
	}

	// projectRoot is what the watchers walk and what .nexusignore patterns
	// are relative to: the directory nexus dev was invoked from.
	projectRoot, _ := os.Getwd()
	userIgnore := loadNexusIgnore(projectRoot)
	if userIgnore != nil {
		fmt.Fprintf(stdout, "  %s● %s · %d pattern(s)%s\n", ansiDim, nexusIgnoreFile, userIgnore.patterns(), ansiReset)
	}

	// --dist: mirror the live frontend into its dist in the background so a
	// `go build` taken mid-session (or the production embed) always matches
	// the current source. Opt-in — it runs a full `vite build` on each
	// debounced change. The plugin keeps the dev server's hot file across
	// the build's emptyOutDir, so the app keeps serving HMR throughout.
	if distWatch && fp.PackageJSON {
		stopDist, err := watchDistBuild(ctx, fp.Dir, nexusTOMLPath(target), vite, userIgnore, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "%s●%s dist watch disabled: %v\n", ansiYellow, ansiReset, err)
		} else {
			defer stopDist()
		}
	} else if distWatch {
		fmt.Fprintf(stderr, "%s●%s --dist needs a frontend with a package.json · ignored\n", ansiYellow, ansiReset)
	}

	var restartCh chan struct{}
	if watch {
		restartCh = make(chan struct{}, 1)
		root := projectRoot
		// In dev mode the frontend dir is owned by Vite's own watcher
		// (or, for a static bundle, read from disk per request); Go has
		// no business rebuilding when its files change. Override the
		// embed-root rule so saves under web/ — and the hot file Vite
		// writes into web/dist — don't bounce the Go process.
		ignore := []string{}
		if frontendDir != "" {
			ignore = append(ignore, frontendDir)
		}
		if err := watchSource(ctx, root, restartCh, stderr, ignore); err != nil {
			fmt.Fprintf(stderr, "watcher disabled: %v\n", err)
			restartCh = nil
		}
	}

	// Session scratch for the state the app preserves across rebuilds
	// (nexus.PreserveDev). It lives outside the repo and dies with the dev
	// session, so in-memory data survives a rebuild but never a Ctrl-C.
	devStatePath := ""
	if dir, err := os.MkdirTemp("", "nexus-dev-state-"); err == nil {
		devStatePath = filepath.Join(dir, "state.json")
		defer os.RemoveAll(dir)
	} else {
		fmt.Fprintf(stderr, "%s●%s dev-state disabled: %v\n", ansiYellow, ansiReset, err)
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
		if op, cl, err := buildDevOverlay(target, distStubRoot); err != nil {
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

		// The first boot's ready line waits for Vite's hot file too, so a
		// browser opened on "ready" loads live modules; later restarts
		// print it as soon as the app answers.
		var viteSettled <-chan struct{}
		if first && vite != nil {
			viteSettled = vite.settledCh()
		}
		ex, kill, err := startDevChild(ctx, binPath, target, addr, overlayPath, devStatePath, openOnReady && first, openDash, verbose, fast, prettyLogs, logFmt, viteSettled, stdout, stderr)
		if err != nil {
			return err
		}
		exited, killChild, running = ex, kill, true
		// Auto-codegen for frontend.Plugin apps. Runs alongside the
		// boot-banner goroutine; both probe the same listen port
		// independently, and devCodegenWatch silently no-ops when there
		// is no frontend dir or no frontend.Plugin is registered on the
		// running app. Each iteration of the loop (Go restart) re-fires
		// the codegen so schema changes flow into the TS tree without a
		// manual `nexus generate frontend`.
		go devCodegenWatch(ctx, addr, frontendDir, "vue", stdout, stderr)
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
func startDevChild(ctx context.Context, binPath, target, addr, overlayPath, devStatePath string, openOnReady, openDash, verbose, fast, prettyLogs bool, logFmt logFormatter, viteSettled <-chan struct{}, stdout, stderr io.Writer) (<-chan error, func(), error) {
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
			args = append(args, "-ldflags=-w -s")
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
		"NEXUS_DEV_ROOT="+targetDir(target),
		"NEXUS_PEER_DEV=1",
		"NEXUS_CONFIG_DEV=1",
	)
	if verbose {
		env = append(env, "NEXUS_VERBOSE=1")
	}
	// Where the app saves the state it preserves across rebuilds
	// (nexus.PreserveDev). Only ever set here, which is what keeps the
	// feature dev-only: a production binary sees no path and does nothing.
	if devStatePath != "" {
		env = append(env, "NEXUS_DEV_STATE="+devStatePath)
	}
	// Nothing says where Vite is: the app reads the hot file the plugin
	// writes (App.ViteHot), the same file this loop waited on.
	cmd.Env = env
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, func() {}, fmt.Errorf("failed to start %s: %w", strings.Join(cmd.Args, " "), err)
	}

	// waitAndOpen runs even when --no-open is set so the user still
	// gets the green "ready" line — only the browser launch is gated
	// on openOnReady. The URL is always the app's: with Vite running
	// the app still serves every page, loading modules from Vite.
	// appDead gates the ready banner. Probing a port only proves something
	// is listening on it — when the app failed to bind (port already taken,
	// a wiring error, a panic) the probe can still succeed against whatever
	// else owns that port, and the banner then advertised an API and a
	// dashboard that were not there.
	var appDead atomic.Bool
	go waitAndOpen(ctx, addr, openOnReady, openDash, stdout, detectedCh, viteSettled, appDead.Load)

	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		appDead.Store(true)
		exited <- err
	}()

	pid := cmd.Process.Pid
	killChild := func() {
		// SIGTERM first to give shutdown handlers (HTTP graceful close,
		// lifecycle hooks) a chance, then SIGKILL after a short grace
		// period. Drain `exited` fully before returning so the caller never
		// has to read from it again — double-reading a buffered chan
		// of size 1 deadlocks (caused Ctrl-C to hang in v0.21.x).
		//
		// The window is deliberately short. A dev app bounds its own
		// shutdown at DevShutdownTimeout (250ms) and cancels in-flight
		// request contexts on the way out, so a healthy app is gone in
		// milliseconds; this only covers one that's genuinely wedged.
		// It used to be 5s, which every restart with an open SSE stream or
		// slow request paid in full — the single largest source of "Ctrl-C
		// takes forever".
		_ = killProcessGroup(pid, syscall.SIGTERM)
		select {
		case <-exited:
			return
		case <-time.After(devKillGrace):
		}
		// Say so — a silent pause reads as a hang, and a wedged shutdown
		// hook is worth knowing about.
		fmt.Fprintf(stderr, "%s●%s app didn't exit within %s · SIGKILL\n", ansiYellow, ansiReset, devKillGrace)
		_ = killProcessGroup(pid, syscall.SIGKILL)
		<-exited
	}
	return exited, killChild, nil
}

// waitAndOpen produces the "ready" line and (optionally) opens the
// browser once the app is up. Two signals race:
//
//  1. The user's Config.Addr — captured by addrFinder from the
//     "nexus: listening on …" log line. Authoritative.
//  2. A periodic probe of the --addr flag value. Fallback for apps
//     that don't print a recognizable listen line (custom routers,
//     fasthttp, etc.).
//
// If detection fires and the address differs from what the user passed
// as --addr, we surface a correction line — a misleading banner is
// the symptom that drove this code, so making the discrepancy
// visible is part of the fix.
//
// viteSettled, when non-nil, holds the line back until the frontend dev
// server has written its hot file (or given up): until then the app would
// serve its built bundle, and a browser opened early would show that.
func waitAndOpen(ctx context.Context, addr string, openBrowserOnReady, openDash bool, stdout io.Writer, detectedCh <-chan string, viteSettled <-chan struct{}, appDead func() bool) {
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

	var ready string
	select {
	case <-ctx.Done():
		return
	case detected := <-detectedCh:
		ready = detected
	case ok := <-flagDone:
		if !ok {
			return
		}
		ready = addr
	}

	if viteSettled != nil {
		select {
		case <-ctx.Done():
			return
		case <-viteSettled:
		}
	}

	// If the user passed an explicit --addr that doesn't match the
	// actual bind, surface the gap. Default --addr (":8080") is
	// suppressed — we never claimed it on the banner anyway, so
	// there's nothing to "correct" for the user.
	if addr != defaultDevAddr && normalizeProbeAddr(ready) != flagAddr {
		fmt.Fprintf(stdout, "\n  %s→ %sbound on %s%s%s %s(--addr was %s)%s\n",
			ansiDim, ansiReset, ansiBold, ready, ansiReset, ansiDim, addr, ansiReset)
	}

	// The run loop has already said the app exited and is waiting for
	// changes; announcing "ready" after that just sends the reader to a
	// port nothing is serving.
	if appDead != nil && appDead() {
		return
	}
	primaryURL := devPrimaryURL(ready, openDash)
	printReadyLine(stdout, primaryURL, openBrowserOnReady)
	if openBrowserOnReady {
		_ = openBrowser(primaryURL)
	}
}

// devPrimaryURL is the URL the ready line advertises (and --open opens):
// always the app, whose origin serves every page — with a Vite dev server
// running, those pages load their modules from it, but nobody browses
// Vite's own port. --open-dash swaps in the app's dashboard.
func devPrimaryURL(ready string, openDash bool) string {
	if openDash {
		return dashboardURL(ready)
	}
	return clientURL(ready)
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
