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
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// newDevCmd builds `nexus dev` — runs `go run` on the target package
// with a startup banner and auto-opens the dashboard once the configured
// port responds. With --split, boots one subprocess per nexus.DeployAs
// tag instead, wiring peer URLs between them so cross-module calls go
// over real HTTP. Cobra wraps the runner.
func newDevCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		addr     string
		noOpen   bool
		openDash bool
		split    bool
		basePort int
		tui      bool
		noWatch  bool
	)
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Run the app with go run + auto-open the dashboard",
		Long: `Boot the user's app via 'go run', print a friendly banner, and
auto-open the dashboard once the listen port responds.

Use this instead of 'go run .' when you want one-command iteration. The
dev runner kills the entire process group on SIGINT/SIGTERM so the
compiled binary doesn't survive Ctrl-C as a zombie.

With --split: discover every nexus.DeployAs(tag) declaration and boot
one subprocess per tag. Each subprocess gets a unique PORT and an
NEXUS_DEPLOYMENT env var; peer URLs are auto-wired via <TAG>_URL so
the codegen'd cross-module clients hit the right peer over real HTTP.

Your main() must read PORT to honor the assignment — see
examples/microsplit for the convention.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "."
			if len(args) > 0 {
				target = args[0]
			}
			if split && tui {
				return errSplitTUI
			}
			if split {
				return runDevSplit(target, basePort, stdout, stderr)
			}
			if tui {
				return runDevTUI(target, addr, openDash, stdout, stderr)
			}
			return runDev(target, addr, !noOpen, openDash, !noWatch, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultDevAddr,
		"dashboard address to probe and open (single-process mode)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false,
		"don't auto-open the browser when the port responds")
	cmd.Flags().BoolVar(&openDash, "open-dash", false,
		"open the /__nexus/ admin dashboard instead of the app's root URL")
	cmd.Flags().BoolVar(&split, "split", false,
		"boot one subprocess per nexus.DeployAs tag (split mode)")
	cmd.Flags().IntVar(&basePort, "base-port", 8080,
		"first port to assign in --split mode (subsequent units take +1, +2, ...)")
	cmd.Flags().BoolVar(&tui, "tui", false,
		"interactive Bubble Tea UI: log pane + restart hotkey + ready indicator")
	cmd.Flags().BoolVar(&noWatch, "no-watch", false,
		"disable file-watch auto-rebuild (single-process mode only)")
	return cmd
}

// defaultDevAddr is the --addr flag's default and the probe target
// when the user doesn't override it. We rely on the framework's
// "nexus: listening on …" output to discover the real bind, so the
// flag is mostly a fallback for non-nexus apps; users running plain
// nexus apps don't need to set it.
const defaultDevAddr = ":8080"

// errSplitTUI surfaces when both --tui and --split are passed. The
// TUI takes over the whole terminal and assumes one child stream;
// driving N subprocesses through it would shred the layout. Refuse
// up front rather than silently degrading.
var errSplitTUI = &userError{"--tui and --split are mutually exclusive (try --split alone with the prefixed log streams)"}

type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

// runDev is the dev-loop body. Separated from the cobra wrapper so the
// happy path (start child → race signal vs natural exit → clean kill)
// reads top-to-bottom without being interleaved with flag parsing.
//
// When watch is true, runs a fsnotify watcher on the target dir and
// restarts `go run` on every coalesced source-file change. SIGINT
// stops the loop and tears down the active child cleanly.
func runDev(target, addr string, openOnReady, openDash, watch bool, stdout, stderr io.Writer) error {
	printDevBanner(stdout, target)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Manifest-aware codegen: when nexus.deploy.yaml exists in cwd,
	// pick the monolith deployment (or first when no monolith), emit
	// zz_deploy_gen.go with that deployment's port + listeners +
	// topology, and feed it through `go run -overlay`. Without this
	// `nexus dev` ran plain `go run .` and the manifest's port +
	// listener config never reached the binary — the framework fell
	// back to its :8080 / single-listener defaults.
	overlayPath, devDeployment, manifestErr := prepareDevOverlay(target)
	if manifestErr != nil {
		fmt.Fprintf(stderr, "manifest codegen skipped: %v\n", manifestErr)
	}

	var restartCh chan struct{}
	if watch {
		restartCh = make(chan struct{}, 1)
		root, _ := os.Getwd()
		if err := watchSource(ctx, root, restartCh, stderr); err != nil {
			fmt.Fprintf(stderr, "watcher disabled: %v\n", err)
			restartCh = nil
		}
	}

	// First boot announces the dashboard URL via waitAndOpen. Subsequent
	// restarts skip the open-browser branch (user already has the tab).
	first := true
	for {
		// Refresh the overlay on every restart so manifest edits and
		// new modules picked up by the watcher land in the next boot.
		// Skipped when the project has no manifest (overlayPath stays
		// "" and `go run` runs without -overlay just like before).
		if !first && manifestErr == nil {
			if p, _, err := prepareDevOverlay(target); err == nil {
				overlayPath = p
			}
		}
		_ = devDeployment // currently informational; reserved for the banner
		exited, killChild, err := startDevChild(ctx, target, addr, overlayPath, openOnReady && first, openDash, stdout, stderr)
		if err != nil {
			return err
		}
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
func startDevChild(ctx context.Context, target, addr, overlayPath string, openOnReady, openDash bool, stdout, stderr io.Writer) (<-chan error, func(), error) {
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
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, func() {}, fmt.Errorf("failed to start `go run %s`: %w", target, err)
	}

	// waitAndOpen runs even when --no-open is set so the user still
	// gets the green "ready" line — only the browser launch is gated
	// on openOnReady.
	go waitAndOpen(ctx, addr, openOnReady, openDash, stdout, detectedCh)

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
func waitAndOpen(ctx context.Context, addr string, openBrowserOnReady, openDash bool, stdout io.Writer, detectedCh <-chan string) {
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

	// If the user passed an explicit --addr that doesn't match the
	// actual bind, surface the gap. Default --addr (":8080") is
	// suppressed — we never claimed it on the banner anyway, so
	// there's nothing to "correct" for the user.
	if addr != defaultDevAddr && normalizeProbeAddr(ready) != flagAddr {
		fmt.Fprintf(stdout, "\n  %s→ %sbound on %s%s%s %s(--addr was %s)%s\n",
			ansiDim, ansiReset, ansiBold, ready, ansiReset, ansiDim, addr, ansiReset)
	}

	url := clientURL(ready)
	if openDash {
		url = dashboardURL(ready)
	}
	printReadyLine(stdout, url, openBrowserOnReady)
	if openBrowserOnReady {
		_ = openBrowser(url)
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
//	nexus: listening on :8080                       ← framework (preferred)
//	[GIN-debug] Listening and serving HTTP on :8080 ← bare-gin user
//	[GIN] Listening and serving HTTPS on :443
//
// First match wins — the framework line lands earlier and reports
// the actual bound address even when the user passed :0.
var ginListenRE = regexp.MustCompile(`(?:nexus: listening on|Listening and serving (?:HTTP|HTTPS) on) (\S+)`)

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

// printDevBanner writes a compact intro block that survives gin's
// debug firehose. We deliberately omit the dashboard URL: at this
// point we don't yet know what address the user's Config.Addr picked
// — printing a guess (the --addr flag's default) and "correcting"
// it later left a stale URL pinned at the top of the terminal even
// after the right one rendered below. The URL appears once, on the
// ready line, after the child binds.
func printDevBanner(w io.Writer, target string) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %snexus dev%s\n", ansiBold, ansiReset)
	fmt.Fprintf(w, "  %s──────────%s\n", ansiDim, ansiReset)
	fmt.Fprintf(w, "  target     %s%s%s\n", ansiBold, target, ansiReset)
	fmt.Fprintf(w, "  %s%s starting · ctrl-c to stop%s\n\n", ansiDim, ansiYellow+"●"+ansiDim, ansiReset)
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
	return exec.Command(name, args...).Start()
}