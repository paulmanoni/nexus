package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// devBuilder compiles the dev target into a private temp dir — one
// binary per build — so the running child keeps serving while the next
// build compiles. That's the whole point of build-then-swap: the user
// pays the process swap (~30ms) on a rebuild instead of the full
// compile+link window, and a build that fails leaves the previous
// binary running instead of taking the app down.
//
// Binaries live outside the source tree (like the handler overlay) so
// the dev loop writes nothing into the user's repo and the file watcher
// can't see its own output.
type devBuilder struct {
	dir  string
	seq  int
	fast bool
}

func newDevBuilder(fast bool) (*devBuilder, error) {
	dir, err := os.MkdirTemp("", "nexus-dev-bin-")
	if err != nil {
		return nil, err
	}
	return &devBuilder{dir: dir, fast: fast}, nil
}

func (b *devBuilder) close() {
	if b == nil || b.dir == "" {
		return
	}
	_ = os.RemoveAll(b.dir)
}

// build compiles target into a fresh binary and returns its path. Each
// build gets its own filename: the previous one is still executing (and
// on Windows still locked), so overwriting in place isn't an option.
// The caller removes the superseded binary once its process is gone.
//
// Compiler diagnostics stream straight to out — a failed build's error
// text is the most important thing on screen, so it isn't reshaped by
// the log prettifier.
//
// Flags mirror what `go run` was invoked with before: -gcflags=all=-N -l
// disables optimization + inlining for the whole graph (markedly faster
// compiles; dev binaries are never perf-sensitive) and -ldflags=-w drops
// DWARF so the linker — the one step no cache makes incremental, and which
// measurement puts at essentially the entire rebuild — has less to emit.
//
// -w is on by default (--debug turns it back off, for delve and full panic
// traces). On a large app it's worth ~20% of every rebuild, and dev binaries
// are thrown away on the next save.
func (b *devBuilder) build(ctx context.Context, target, overlayPath string, out io.Writer) (string, error) {
	b.seq++
	bin := filepath.Join(b.dir, fmt.Sprintf("app-%d%s", b.seq, exeSuffix()))

	args := []string{"build"}
	if overlayPath != "" {
		args = append(args, "-overlay="+overlayPath)
	}
	args = append(args, "-gcflags=all=-N -l")
	if b.fast {
		args = append(args, "-ldflags=-w")
	}
	// Compile from inside the target's own directory when it resolves to a
	// real path, so a target outside the CLI's module (or in a nested one)
	// builds against the right module. The child process still inherits the
	// CLI's working directory, so nexus.toml resolution is unaffected.
	buildDir, pkg := "", target
	if fi, err := os.Stat(target); err == nil {
		if fi.IsDir() {
			buildDir, pkg = target, "."
		} else {
			buildDir, pkg = filepath.Dir(target), filepath.Base(target)
		}
	}
	args = append(args, "-o", bin, pkg)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = buildDir
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		_ = os.Remove(bin)
		return "", err
	}
	return bin, nil
}

// prewarm executes the freshly built binary once, with a deliberately
// malformed GOMEMLIMIT, and throws the process away.
//
// Why: the first exec of a never-executed file costs ~450ms on macOS
// (AMFI validates the code signature and caches the result per file);
// every later exec of the same file is ~25ms. Paying that inside the
// swap window would dominate the restart — so we pay it here, while the
// previous child is still serving.
//
// The malformed value makes the Go runtime abort in schedinit, before
// package inits and before main, so none of the app's own code runs: no
// port bind, no DB dial, no side effects. NEXUS_DEV_PREWARM is a belt-
// and-braces marker for framework-side early exits; the context deadline
// plus the process-group kill guarantee the probe can't outlive the
// swap even if a future runtime stops rejecting the value.
//
// Best-effort throughout: a prewarm that fails just means the next exec
// pays full price.
func (b *devBuilder) prewarm(ctx context.Context, bin string) {
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, bin)
	cmd.Env = append(os.Environ(),
		"GOMEMLIMIT=nexus-dev-prewarm",
		"NEXUS_DEV_PREWARM=1",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	setProcessGroup(cmd)
	_ = cmd.Run()
}

// exeSuffix is the platform's executable extension. Without it Windows
// refuses to exec the freshly built file.
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// fileHash returns the SHA-256 of a file's contents. The dev loop
// compares consecutive builds with it: Go's build output is
// deterministic, so identical bytes mean the running process already IS
// this build (a _test.go save, an editor writing an unchanged buffer, an
// edit in a package the app doesn't import) and the restart is skipped.
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// future is a value computed off the critical path: the work starts when
// the future is created and get() blocks only when the answer is finally
// needed. `nexus dev` uses it for the startup probes that used to run in
// sequence ahead of the first compile — the Inertia detection's `go list`
// and the viteless dev server's boot — neither of which the build depends
// on.
type future[T any] struct {
	done chan struct{}
	val  T
}

func newFuture[T any](fn func() T) *future[T] {
	f := &future[T]{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		f.val = fn()
	}()
	return f
}

func (f *future[T]) get() T {
	<-f.done
	return f.val
}
