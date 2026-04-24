package main

import (
	"context"
	"flag"
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
)

// cmdDev boots the user's app with `go run`, prints a startup banner, and
// opens the dashboard once the configured port responds. It's a niceness
// wrapper — the user can always just `go run .` directly; `nexus dev` exists
// so the common-case loop (run → open tab) is one command.
func cmdDev(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dev", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":8080", "dashboard address to probe and open")
	noOpen := fs.Bool("no-open", false, "don't auto-open the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}

	fmt.Fprintf(stdout, "nexus dev: running %q (dashboard → %s)\n", target, dashboardURL(*addr))

	// Wire SIGINT/SIGTERM to the subprocess so Ctrl-C shuts the server down
	// cleanly (otherwise `go run` receives the signal but the child binary
	// it spawned keeps running until its own handler fires).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, "go", "run", target)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	// Propagate signals to the whole process group so gin's own shutdown
	// handlers run (Darwin + Linux only; harmless no-op elsewhere).
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start `go run %s`: %w", target, err)
	}

	// Probe the port in the background; open the browser as soon as we
	// get a TCP handshake. Give up after 30s to avoid hanging forever on
	// a handler-less app.
	if !*noOpen {
		go waitAndOpen(ctx, *addr, stdout)
	}

	err := cmd.Wait()
	// Exit code 0 on clean shutdown (SIGINT/SIGTERM is expected here),
	// non-zero on a real crash.
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("app exited: %w", err)
	}
	return nil
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