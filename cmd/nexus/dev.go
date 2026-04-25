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
// port responds. Cobra wraps the runner.
func newDevCmd(stdout, stderr io.Writer) *cobra.Command {
	var addr string
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Run the app with go run + auto-open the dashboard",
		Long: `Boot the user's app via 'go run', print a friendly banner, and
auto-open the dashboard once the listen port responds.

Use this instead of 'go run .' when you want one-command iteration. The
dev runner kills the entire process group on SIGINT/SIGTERM so the
compiled binary doesn't survive Ctrl-C as a zombie.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			target := "."
			if len(args) > 0 {
				target = args[0]
			}
			return runDev(target, addr, !noOpen, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", ":8080",
		"dashboard address to probe and open")
	cmd.Flags().BoolVar(&noOpen, "no-open", false,
		"don't auto-open the browser when the port responds")
	return cmd
}

// runDev is the dev-loop body. Separated from the cobra wrapper so the
// happy path (start child → race signal vs natural exit → clean kill)
// reads top-to-bottom without being interleaved with flag parsing.
func runDev(target, addr string, openOnReady bool, stdout, stderr io.Writer) error {
	fmt.Fprintf(stdout, "nexus dev: running %q (dashboard → %s)\n", target, dashboardURL(addr))

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

	if openOnReady {
		go waitAndOpen(ctx, addr, stdout)
	}

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
func waitAndOpen(ctx context.Context, addr string, stdout io.Writer) {
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
			fmt.Fprintf(stdout, "nexus dev: opening %s\n", url)
			_ = openBrowser(url)
			return
		}
		time.Sleep(200 * time.Millisecond)
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