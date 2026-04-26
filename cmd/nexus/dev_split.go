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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runDevSplit boots one subprocess per deployment unit. Detects the
// deploy manifest under target's directory:
//
//   - With a manifest (Path 3): builds a separate binary per unit
//     using `nexus build --deployment X` so each subprocess has the
//     right shadow code compiled in. checkout-svc's binary contains
//     the HTTP-stub *users.Service; users-svc's contains the real one.
//   - Without a manifest: builds one binary, runs N subprocesses with
//     NEXUS_DEPLOYMENT env vars and runtime module filtering — the
//     pre-Path-3 behavior, kept so projects that haven't migrated
//     still work.
//
// Either way, peer URLs are auto-wired via <TAG>_URL env vars (which
// main.go threads into Config.Topology.Peers), so cross-module calls
// reach the right subprocess.
func runDevSplit(target string, basePort int, stdout, stderr io.Writer) error {
	if manifestPath := findManifest(target); manifestPath != "" {
		return runDevSplitWithManifest(manifestPath, target, basePort, stdout, stderr)
	}
	return runDevSplitLegacy(target, basePort, stdout, stderr)
}

// findManifest looks for nexus.deploy.yaml next to target (or in target
// itself when target is a directory). Returns "" when no manifest
// is found — the caller falls back to the legacy single-binary path.
func findManifest(target string) string {
	candidates := []string{
		filepath.Join(target, "nexus.deploy.yaml"),
		filepath.Join(filepath.Dir(target), "nexus.deploy.yaml"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// runDevSplitWithManifest is the Path-3-aware splitter. Reads the
// manifest, picks every deployment whose name matches a DeployAs tag
// (the monolith deployment is skipped — it owns everything and isn't
// a split unit by definition), and builds one binary per unit via
// `runBuild` so each gets the right shadow code.
func runDevSplitWithManifest(manifestPath, target string, basePort int, stdout, stderr io.Writer) error {
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	tags, err := discoverDeployTags(scanPattern(target))
	if err != nil {
		return fmt.Errorf("discover DeployAs tags: %w", err)
	}
	if len(tags) == 0 {
		return fmt.Errorf("no nexus.DeployAs(...) tags found in %s — split mode needs at least one tagged module", target)
	}
	tagSet := map[string]bool{}
	for _, t := range tags {
		tagSet[t] = true
	}

	// Split units = deployments whose name matches a DeployAs tag in
	// source OR whose manifest entry has a non-empty owns list (the
	// auto-inject path: manifest declares the unit, codegen registers
	// the tag on every owned module's nexus.Module call). Monolith
	// (empty owns) is never a split unit.
	var splitDeployments []string
	for name, spec := range manifest.Deployments {
		isSplit := tagSet[name] || len(spec.Owns) > 0
		if isSplit {
			splitDeployments = append(splitDeployments, name)
		}
	}
	if len(splitDeployments) < 2 {
		return fmt.Errorf("split mode needs ≥ 2 deployments whose names match DeployAs tags in %s (found %d) — add deployment entries for each unit",
			manifestPath, len(splitDeployments))
	}
	sort.Strings(splitDeployments)

	// Use manifest-defined ports when present; fall back to
	// --base-port auto-assignment for tags that don't declare one.
	units := planUnitsFromManifest(splitDeployments, manifest, basePort)

	if busy := portsInUse(units); len(busy) > 0 {
		return fmt.Errorf("split mode: ports already in use: %s — pass --base-port to shift the assignment",
			strings.Join(busy, ", "))
	}

	printSplitBanner(stdout, units)

	// Build one binary per unit via the overlay path. Each unit's
	// binary has its non-owned modules shadowed — so checkout-svc
	// gets the HTTP-stub *users.Service even though both source
	// trees are the same on disk.
	tmpRoot, err := os.MkdirTemp("", "nexus-split-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpRoot)

	binByTag := map[string]string{}
	for _, u := range units {
		binPath := filepath.Join(tmpRoot, u.Tag)
		fmt.Fprintf(stdout, "  %sbuilding %s%s\n", ansiDim, u.Tag, ansiReset)
		err := runBuild(buildOptions{
			Deployment:   u.Tag,
			ManifestPath: manifestPath,
			Output:       binPath,
			MainPackage:  target,
			// Suppress the per-build overlay/shadow chatter so the
			// dev banner stays scannable. Errors still flow through
			// stderr so build failures surface.
			Stdout: io.Discard,
			Stderr: stderr,
		})
		if err != nil {
			return fmt.Errorf("build %s: %w", u.Tag, err)
		}
		binByTag[u.Tag] = binPath
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var procs []*exec.Cmd
	for _, u := range units {
		cmd := startUnit(ctx, binByTag[u.Tag], u, units, stdout, stderr)
		if err := cmd.Start(); err != nil {
			killAll(procs)
			return fmt.Errorf("start %s: %w", u.Tag, err)
		}
		procs = append(procs, cmd)
	}

	return waitForSplit(ctx, procs, units, stderr)
}

// runDevSplitLegacy is the pre-Path-3 splitter — kept so projects
// without a deploy manifest still get split mode. Builds one binary
// containing every module; subprocesses use NEXUS_DEPLOYMENT for
// runtime module filtering and pre-codegen RemoteCaller env-var
// peer wiring.
func runDevSplitLegacy(target string, basePort int, stdout, stderr io.Writer) error {
	tags, err := discoverDeployTags(scanPattern(target))
	if err != nil {
		return fmt.Errorf("discover DeployAs tags: %w", err)
	}
	if len(tags) == 0 {
		return fmt.Errorf("no nexus.DeployAs(...) tags found in %s — split mode needs at least one tagged module", target)
	}
	if len(tags) == 1 {
		return fmt.Errorf("split mode needs ≥ 2 DeployAs tags (found 1: %q) — use `nexus dev` for a single deployment", tags[0])
	}

	units := planUnits(tags, basePort)

	if busy := portsInUse(units); len(busy) > 0 {
		return fmt.Errorf("split mode: ports already in use: %s — pass --base-port to shift the assignment",
			strings.Join(busy, ", "))
	}

	printSplitBanner(stdout, units)

	bin, cleanup, err := buildBinary(target, stdout, stderr)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var procs []*exec.Cmd
	for _, u := range units {
		cmd := startUnit(ctx, bin, u, units, stdout, stderr)
		if err := cmd.Start(); err != nil {
			killAll(procs)
			return fmt.Errorf("start %s: %w", u.Tag, err)
		}
		procs = append(procs, cmd)
	}

	return waitForSplit(ctx, procs, units, stderr)
}

// waitForSplit is the shared shutdown loop both splitter variants
// use: race a SIGINT against any unit's natural exit; either signal
// kills the rest so the user isn't left with a half-running topology.
//
// Also kicks off per-unit readiness probes — when each unit's port
// responds, prints a green dot with the URL so the user has an
// unambiguous "go ahead" signal even when the subprocess output is
// quiet (e.g. with GIN_MODE=release). Once every unit is ready, a
// summary line lands so the user can hit Enter and start curling.
func waitForSplit(ctx context.Context, procs []*exec.Cmd, units []unit, stderr io.Writer) error {
	go awaitReady(ctx, units, stderr)

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

// planUnitsFromManifest uses each deployment's manifest port when
// non-zero; otherwise falls back to basePort auto-assignment for that
// unit. The fallback runs through the same +i offset logic so two
// unported tags don't collide.
func planUnitsFromManifest(tags []string, manifest *DeployManifest, basePort int) []unit {
	units := make([]unit, len(tags))
	for i, t := range tags {
		port := 0
		if spec, ok := manifest.Deployments[t]; ok && spec.Port > 0 {
			port = spec.Port
		}
		if port == 0 {
			port = basePort + i
		}
		units[i] = unit{
			Tag:    t,
			Port:   port,
			URL:    fmt.Sprintf("http://localhost:%d", port),
			EnvVar: tagToURLEnv(t),
		}
	}
	return units
}

// portsInUse returns the host:port strings for every unit whose
// port already has a listener. The probe is cheap (50ms TCP dial);
// false positives from a third-party scanner racing the dial are
// vanishingly rare and not worth defending against in a dev tool.
func portsInUse(units []unit) []string {
	var busy []string
	for _, u := range units {
		addr := fmt.Sprintf("localhost:%d", u.Port)
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			busy = append(busy, fmt.Sprintf("%d (%s)", u.Port, u.Tag))
		}
	}
	return busy
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

// tagToURLEnv is the convention the splitter uses to wire peer URLs:
// "users-svc" → "USERS_SVC_URL". The same convention is mirrored by
// the deploy-init codegen (cmd/nexus/deploy_init.go:tagToURLEnvVar)
// so a binary built via `nexus build` reads the same env var the
// splitter sets.
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
		// Silence fx's PROVIDE/INVOKE/HOOK chatter in subprocess
		// stdout — split mode already prefixes every line with a
		// colored tag, doubling that with fx's banner makes the
		// terminal hard to scan. Users hitting framework-level
		// issues can unset this in their env.
		"NEXUS_FX_QUIET=1",
		// Switch gin to release mode so the per-subprocess route
		// table dump (~20 [GIN-debug] lines per unit, multiplied by
		// N units in split mode) doesn't drown the banner. The user
		// still sees nexus's own "listening on" line and any
		// per-request logs the app installs explicitly.
		"GIN_MODE=release",
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

// awaitReady polls each unit's /__nexus/config endpoint until it
// responds 200, printing a per-unit ready line as soon as it does.
// When every unit is up, prints a single summary line (`● all ready
// · N units`) so the user has a clear "you can curl now" signal.
//
// Polls with 100ms cadence and a 30s overall ceiling. If a unit is
// still not responding after the ceiling, no message is printed for
// it — the subprocess's own logs (or an exit) tell the rest of the
// story.
func awaitReady(ctx context.Context, units []unit, w io.Writer) {
	ready := make(chan int, len(units))
	for i, u := range units {
		i, u := i, u
		go func() {
			if probeUntilReady(ctx, u, 30*time.Second) {
				color := tagColor(i)
				fmt.Fprintf(w, "  %s●%s %s%-12s%s ready · %s%s%s\n",
					ansiGreen, ansiReset, color, u.Tag, ansiReset, ansiCyan, u.URL, ansiReset)
				// Once the port answers, subscribe to the unit's
				// trace-event stream so per-request lines and
				// cross-service span markers flow into the terminal.
				// The goroutine runs until ctx is cancelled — same
				// lifetime as the splitter overall.
				go streamUnitEvents(ctx, u, i, w)
				ready <- i
			} else {
				ready <- -1
			}
		}()
	}
	got := 0
	for i := 0; i < len(units); i++ {
		select {
		case <-ctx.Done():
			return
		case idx := <-ready:
			if idx >= 0 {
				got++
			}
		}
	}
	if got == len(units) {
		fmt.Fprintf(w, "\n  %s●%s all ready · %d units · %sctrl-c to stop%s\n\n",
			ansiGreen, ansiReset, got, ansiDim, ansiReset)
	}
}

// probeUntilReady returns true when the unit's port answers a TCP
// dial (the cheapest "is anything listening" probe). Any HTTP-level
// error means the listener is up — that's what we care about for
// the ready signal.
func probeUntilReady(ctx context.Context, u unit, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("localhost:%d", u.Port)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
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