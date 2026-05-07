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
// Lifecycle:
//   - Child process group is killed on ctx cancel (the same SIGINT
//     that tears down the Go run loop), so a single Ctrl-C cleans
//     up everything.
//   - Survives across Go restarts. The frontend toolchain has its
//     own incremental compile + file watcher; bouncing it on every
//     Go save would cripple iteration feel.
//   - When the child exits unexpectedly, we log it but don't fail
//     nexus dev — the user can fix the script and relaunch.
func startFrontendWatcher(ctx context.Context, dir, cmdline string, stdout, stderr io.Writer) error {
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
	wg.Add(2)
	go pump(stdoutPipe, stdout)
	go pump(stderrPipe, stderr)

	go func() {
		wg.Wait()
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "%sfrontend watcher exited: %v%s\n", ansiYellow, err, ansiReset)
		}
	}()

	fmt.Fprintf(stdout, "%s●%s frontend watcher: %q in %s\n", ansiCyan, ansiReset, cmdline, dir)
	return nil
}