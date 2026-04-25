package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runDevSplit boots one subprocess per nexus.DeployAs tag discovered in
// target. Each subprocess gets its own port and a NEXUS_DEPLOYMENT env
// var; the framework's existing Config.Deployment + DeploymentFromEnv()
// pattern in user code reads the variable and selects the matching
// modules. Cross-module clients in each subprocess find their peers via
// auto-injected `<TAG>_URL` environment variables — same convention
// the generated RemoteCaller already reads.
//
// The user's main() must read PORT (or whatever knob it exposes) and
// pass it into Config.Addr. The microsplit example shows the
// convention; nothing forces it, but a binary that ignores PORT will
// have all subprocesses fight for :8080 and crash all but one.
func runDevSplit(target string, basePort int, stdout, stderr io.Writer) error {
	// Discovery has to walk every subpackage — modules typically live
	// under foo/users, foo/checkout, etc. — but `go build` should still
	// receive the user's literal target (so `./examples/microsplit`
	// builds main, not main + everything below). Append /... only for
	// the AST scan.
	tags, err := discoverDeployTags(scanPattern(target))
	if err != nil {
		return fmt.Errorf("discover DeployAs tags: %w", err)
	}
	if len(tags) == 0 {
		return fmt.Errorf("no nexus.DeployAs(...) tags found in %s — split mode needs at least one tagged module", target)
	}
	if len(tags) == 1 {
		// One tag = one process = exactly the same as `nexus dev`.
		// Tell the user explicitly so they don't think split mode
		// silently degraded; the explicit error nudges them to either
		// add a second tag or drop the flag.
		return fmt.Errorf("split mode needs ≥ 2 DeployAs tags (found 1: %q) — use `nexus dev` for a single deployment", tags[0])
	}

	units := planUnits(tags, basePort)
	printSplitBanner(stdout, units)

	// Build the binary once up front. Cheaper than `go run` per unit
	// (it would compile N times) and gives clearer errors: a build
	// failure surfaces here, before any subprocess starts.
	bin, cleanup, err := buildBinary(target, stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start every unit. If any fails to start, kill the ones we
	// already started — partial-launch states are confusing.
	var procs []*exec.Cmd
	for _, u := range units {
		cmd := startUnit(ctx, bin, u, units, stdout, stderr)
		if err := cmd.Start(); err != nil {
			killAll(procs)
			return fmt.Errorf("start %s: %w", u.Tag, err)
		}
		procs = append(procs, cmd)
	}

	// Wait for any unit to exit, OR signal. Either is the cue to
	// shut everyone down — the user expects all-or-none in dev.
	exited := make(chan exitMsg, len(procs))
	for i, p := range procs {
		i, p := i, p
		go func() { exited <- exitMsg{tag: units[i].Tag, err: p.Wait()} }()
	}

	select {
	case msg := <-exited:
		// One unit died — kill the rest so the user isn't left with
		// half a topology.
		fmt.Fprintf(stderr, "\n  %s● %s%s exited (%v) — stopping siblings%s\n",
			ansiYellow, ansiReset, msg.tag, msg.err, ansiReset)
		killAll(procs)
		drainExits(exited, len(procs)-1)
		return msg.err
	case <-ctx.Done():
		killAll(procs)
		drainExits(exited, len(procs))
		return nil
	}
}

// unit is one process the splitter is going to start. The fields are
// pre-computed before any process spawns so the banner can render the
// full plan up front (and so URL wiring sees every peer's port).
type unit struct {
	Tag    string // DeployAs(tag)
	Port   int    // base + index
	URL    string // http://localhost:<port>
	EnvVar string // <TAG>_URL — the env name peers read
}

func planUnits(tags []string, basePort int) []unit {
	units := make([]unit, len(tags))
	for i, t := range tags {
		port := basePort + i
		units[i] = unit{
			Tag:    t,
			Port:   port,
			URL:    fmt.Sprintf("http://localhost:%d", port),
			EnvVar: tagToURLEnv(t),
		}
	}
	return units
}

// scanPattern turns a target path into a recursive Go pattern. If the
// caller already passed a "..." pattern we honor it verbatim;
// otherwise we extend a plain path to include subpackages so module
// declarations under foo/users, foo/checkout, etc. all surface.
func scanPattern(target string) string {
	if target == "" {
		return "./..."
	}
	if strings.Contains(target, "...") {
		return target
	}
	return strings.TrimSuffix(target, "/") + "/..."
}

// tagToURLEnv mirrors the convention the codegen bakes in via
// envVarFromTag — keep them in lockstep so a freshly-generated
// RemoteCaller picks up the var the splitter sets. ("users-svc" →
// "USERS_SVC_URL")
func tagToURLEnv(tag string) string {
	out := strings.ToUpper(tag)
	out = strings.ReplaceAll(out, "-", "_")
	return out + "_URL"
}

// buildBinary compiles target to a temp executable. Returns the path
// plus a cleanup func that removes the temp file on shutdown.
func buildBinary(target string, stdout, stderr io.Writer) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "nexus-split-")
	if err != nil {
		return "", nil, err
	}
	bin := filepath.Join(tmp, "app")
	cmd := exec.Command("go", "build", "-o", bin, target)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	fmt.Fprintf(stdout, "  %sbuilding %s%s\n", ansiDim, target, ansiReset)
	if err := cmd.Run(); err != nil {
		os.RemoveAll(tmp)
		return "", nil, fmt.Errorf("go build %s: %w", target, err)
	}
	return bin, func() { os.RemoveAll(tmp) }, nil
}

// startUnit assembles a *exec.Cmd for one unit. The env carries:
//   - NEXUS_DEPLOYMENT — selects which DeployAs-tagged modules this
//     binary considers "local" via Config.Deployment.
//   - PORT — the user's main() reads this to pick Config.Addr.
//   - <PEER_TAG>_URL for every other unit, so the codegen'd
//     RemoteCallerFromEnv() finds its peer.
//
// stdout and stderr go through prefixWriter so concurrent log lines
// from all units stay readable: each line prefixed with the colored
// tag name.
func startUnit(_ context.Context, bin string, u unit, all []unit, stdout, stderr io.Writer) *exec.Cmd {
	env := os.Environ()
	env = append(env,
		"NEXUS_DEPLOYMENT="+u.Tag,
		fmt.Sprintf("PORT=%d", u.Port),
	)
	for _, peer := range all {
		if peer.Tag == u.Tag {
			continue
		}
		env = append(env, peer.EnvVar+"="+peer.URL)
	}

	prefix := tagPrefix(u.Tag, indexOf(u.Tag, all))
	cmd := exec.Command(bin)
	cmd.Env = env
	cmd.Stdout = newPrefixWriter(stdout, prefix)
	cmd.Stderr = newPrefixWriter(stderr, prefix)
	setProcessGroup(cmd)
	return cmd
}

func indexOf(tag string, units []unit) int {
	for i, u := range units {
		if u.Tag == tag {
			return i
		}
	}
	return 0
}

// killAll sends SIGTERM to every process group, waits a grace period,
// then SIGKILL. Mirrors the shape `runDev` uses for one process —
// the only difference is the loop.
func killAll(procs []*exec.Cmd) {
	for _, p := range procs {
		if p == nil || p.Process == nil {
			continue
		}
		_ = killProcessGroup(p.Process.Pid, syscall.SIGTERM)
	}
	deadline := time.After(5 * time.Second)
	for {
		allGone := true
		for _, p := range procs {
			if p == nil || p.Process == nil {
				continue
			}
			if err := p.Process.Signal(syscall.Signal(0)); err == nil {
				allGone = false
				break
			}
		}
		if allGone {
			return
		}
		select {
		case <-deadline:
			for _, p := range procs {
				if p == nil || p.Process == nil {
					continue
				}
				_ = killProcessGroup(p.Process.Pid, syscall.SIGKILL)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// drainExits consumes the remaining exit messages from the children
// killed by killAll so cmd.Wait()'s goroutines don't leak past
// runDevSplit's return.
func drainExits(ch <-chan exitMsg, n int) {
	for i := 0; i < n; i++ {
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			return
		}
	}
}

type exitMsg struct {
	tag string
	err error
}

// --- Banner ---

func printSplitBanner(w io.Writer, units []unit) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %snexus dev %s(split mode)%s\n", ansiBold, ansiDim, ansiReset)
	fmt.Fprintf(w, "  %s──────────%s\n", ansiDim, ansiReset)
	for i, u := range units {
		color := tagColor(i)
		fmt.Fprintf(w, "  %s%-12s%s  port %d  →  %s%s%s\n",
			color, u.Tag, ansiReset, u.Port, ansiCyan, u.URL, ansiReset)
	}
	fmt.Fprintf(w, "  %s%s starting · ctrl-c to stop all%s\n\n",
		ansiDim, ansiYellow+"●"+ansiDim, ansiReset)
}

// --- Tag-prefixed log writer ---

// prefixWriter buffers child output until newlines, then emits each
// line prefixed with `[tag] `. Without buffering, two children writing
// concurrently produce shredded mid-line output that's painful to
// scan.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
	buf    bytes.Buffer
}

func newPrefixWriter(w io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{w: w, prefix: prefix}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	for {
		raw := p.buf.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := raw[:i+1]
		// Write atomically with one Fprintf so concurrent writers
		// don't shred the prefix off of the line content.
		fmt.Fprintf(p.w, "%s %s", p.prefix, string(line))
		p.buf.Next(i + 1)
	}
	return len(b), nil
}

// --- Tag colors ---

// tagColors cycles through ANSI hues so each unit's prefix and banner
// row share the same color. Six is enough for typical micro-splits;
// beyond that it wraps — better than running out and printing plain.
var tagColors = []string{
	"\033[36m", // cyan
	"\033[33m", // yellow
	"\033[35m", // magenta
	"\033[32m", // green
	"\033[34m", // blue
	"\033[31m", // red
}

func tagColor(i int) string { return tagColors[i%len(tagColors)] }

func tagPrefix(tag string, i int) string {
	return fmt.Sprintf("%s%s%s", tagColor(i), padTag(tag, 12), ansiReset)
}

// padTag right-pads a tag to width so the streamed log lines align in
// the terminal. Tags longer than width get truncated with an ellipsis;
// tags exactly equal to width fit verbatim (no truncation surprise).
func padTag(tag string, width int) string {
	if len(tag) > width {
		return tag[:width-1] + "…"
	}
	return tag + strings.Repeat(" ", width-len(tag))
}