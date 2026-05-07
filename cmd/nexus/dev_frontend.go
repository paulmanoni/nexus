package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// startFrontendWatcher spawns the user's frontend build watcher
// (typically `vite build --watch` wrapped in an npm script) inside
// dir, prefixing every output line with [web] so the combined log
// stream stays readable.
//
// quiet=true silences the child's stdout (the per-build "✓ N
// modules transformed" + asset-table spam) while keeping stderr
// streaming so real errors still surface. Build-progress output
// gets noisy fast under `vite build --watch`, especially when an
// auto-import plugin pegs the watcher into a self-rebuild loop —
// we'd rather hide the spam than let it drown the interactive
// session. Pass --verbose on `nexus dev` to keep the stream.
//
// Lifecycle:
//   - Child process group is killed on ctx cancel (the same SIGINT
//     that tears down the Go run loop), so a single Ctrl-C cleans
//     up everything.
//   - Survives across Go restarts. The frontend toolchain has its
//     own incremental compile + file watcher; bouncing it on every
//     Go save would cripple iteration feel.
//   - When the child exits unexpectedly, we log it but don't fail
//     nexus dev — the user can fix the script and relaunch.
func startFrontendWatcher(ctx context.Context, dir, cmdline string, quiet bool, stdout, stderr io.Writer) error {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return fmt.Errorf("--frontend-cmd is empty")
	}
	// Honor the user's shell so quoted args + npm scripts that
	// fork their own child processes work without a Go-side parser.
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Dir = dir
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	setProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %q in %s: %w", cmdline, dir, err)
	}

	tag := fmt.Sprintf("%s[web]%s ", ansiCyan, ansiReset)
	var wg sync.WaitGroup
	pump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		// Vite output occasionally exceeds the default 64K line
		// limit when it dumps a chunk graph at boot — bump the
		// buffer so we don't truncate.
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintf(dst, "%s%s\n", tag, scanner.Text())
		}
	}
	// Always drain the pipe so the child doesn't block on a full
	// stdout buffer — quiet mode just discards what it reads.
	stdoutSink := stdout
	if quiet {
		stdoutSink = io.Discard
	}
	wg.Add(2)
	go pump(stdoutPipe, stdoutSink)
	go pump(stderrPipe, stderr)

	go func() {
		wg.Wait()
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "%sfrontend watcher exited: %v%s\n", ansiYellow, err, ansiReset)
		}
	}()

	fmt.Fprintf(stdout, "%s●%s frontend watcher: %q in %s\n", ansiCyan, ansiReset, cmdline, dir)
	if quiet {
		fmt.Fprintf(stdout, "  %s(build output suppressed — pass --verbose to stream %q logs)%s\n", ansiDim, "[web]", ansiReset)
	}
	return nil
}