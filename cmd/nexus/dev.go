package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
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
		split    bool
		basePort int
		tui      bool
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
				return runDevTUI(target, addr, stdout, stderr)
			}
			return runDev(target, addr, !noOpen, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080",
		"dashboard address to probe and open (single-process mode)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false,
		"don't auto-open the browser when the port responds")
	cmd.Flags().BoolVar(&split, "split", false,
		"boot one subprocess per nexus.DeployAs tag (split mode)")
	cmd.Flags().IntVar(&basePort, "base-port", 8080,
		"first port to assign in --split mode (subsequent units take +1, +2, ...)")
	cmd.Flags().BoolVar(&tui, "tui", false,
		"interactive Bubble Tea UI: log pane + restart hotkey + ready indicator")
	return cmd
}

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
func runDev(target, addr string, openOnReady bool, stdout, stderr io.Writer) error {
	printDevBanner(stdout, target, addr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// We deliberately do NOT use exec.CommandContext: that cancels by
	// killing only the direct child PID — the `go` process — leaving
	// the compiled binary `go run` exec'd as an orphan that keeps the
	// listening port bound. Instead we manage shutdown explicitly via
	// the process group below so the binary actually dies on Ctrl-C.
	cmd := exec.Command("go", "run", target)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start `go run %s`: %w", target, err)
	}

	// waitAndOpen runs even when --no-open is set so the user still
	// gets the green "ready" line — only the browser launch is gated
	// on openOnReady.
	go waitAndOpen(ctx, addr, openOnReady, stdout)

	// Wait in a goroutine so the main goroutine can race the user's
	// signal against the child's natural exit.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		// Child terminated on its own (build error, panic, normal exit).
		if err != nil {
			return fmt.Errorf("app exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		// User interrupted — kill the whole process group so the
		// compiled binary dies along with the `go` wrapper. SIGTERM
		// first to give shutdown handlers (HTTP graceful close, fx
		// hooks) a chance, then SIGKILL after a short grace period.
		_ = killProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = killProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		return nil
	}
}

// waitAndOpen polls addr every 200ms until it accepts a connection, then
// opens the dashboard. Bounded by timeout so a broken app doesn't hang
// this goroutine forever.
func waitAndOpen(ctx context.Context, addr string, openBrowserOnReady bool, stdout io.Writer) {
	deadline := time.Now().Add(30 * time.Second)
	probe := normalizeProbeAddr(addr)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := net.DialTimeout("tcp", probe, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			url := dashboardURL(addr)
			printReadyLine(stdout, url, openBrowserOnReady)
			if openBrowserOnReady {
				_ = openBrowser(url)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

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
)

// printDevBanner writes a 5-line block that survives the gin debug
// firehose: a clear title, the target + dashboard URL, and the
// keybind hint. Spacing above and below sets it apart from the
// child's first lines of output.
func printDevBanner(w io.Writer, target, addr string) {
	url := dashboardURL(addr)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %snexus dev%s\n", ansiBold, ansiReset)
	fmt.Fprintf(w, "  %s──────────%s\n", ansiDim, ansiReset)
	fmt.Fprintf(w, "  target     %s%s%s\n", ansiBold, target, ansiReset)
	fmt.Fprintf(w, "  dashboard  %s%s%s\n", ansiCyan, url, ansiReset)
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

// normalizeProbeAddr turns a Gin-style listen spec (":8080") into a dialable
// host:port. Bare ports get "localhost" prefixed so net.Dial doesn't reject.
func normalizeProbeAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}

// dashboardURL renders the full dashboard URL for the banner. Mirrors
// normalizeProbeAddr's localhost rewrite so the printed link works when
// addr is just ":8080".
func dashboardURL(addr string) string {
	host := normalizeProbeAddr(addr)
	return "http://" + host + "/__nexus/"
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