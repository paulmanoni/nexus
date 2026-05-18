package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/evanw/esbuild/pkg/api"

	"github.com/paulmanoni/nexus/frontend/deps/bundler"
	"github.com/paulmanoni/nexus/frontend/deps/lockfile"
	"github.com/paulmanoni/nexus/frontend/deps/resolver"
	"github.com/paulmanoni/nexus/frontend/deps/store"
)

// viteURLRE matches the "Local:" line vite's dev server prints
// when ready, e.g. "  ➜  Local:   http://localhost:5173/".
// Capture group 1 is the URL.
var viteURLRE = regexp.MustCompile(`Local:\s*(https?://[^\s/]+/?)`)

// startFrontendWatcher dispatches between the two frontend-watch
// implementations based on what it finds in dir:
//
//   - nexus.lock present → bundler mode. Drives our esbuild
//     watcher directly via bundler.Build(Watch: true), no
//     subprocess. cmdline is ignored in this mode (we know what
//     to do without being told). frontendURLCh never receives a
//     URL — dev.go's waitAndOpen falls back to the gin URL after
//     its grace window, which is correct because bundler mode
//     produces on-disk islands/ files Go serves itself.
//
//   - nexus.lock absent → legacy vite mode. Same behavior as
//     before: spawn the user's `npm run dev` (or whatever
//     cmdline says), sniff stdout for the Local: URL, dedup
//     identical rebuild blocks, surface errors. Kept so projects
//     that haven't migrated to the deps system still work
//     unchanged.
//
// Mode detection runs once at startup. We don't try to switch
// modes mid-session if a user adds nexus.lock; that's a restart-
// nexus-dev moment, not a runtime concern.
func startFrontendWatcher(ctx context.Context, dir, cmdline string, verbose bool, stdout, stderr io.Writer, frontendURLCh chan<- string) error {
	if depsModeAvailable(dir) {
		return startBundlerWatcher(ctx, dir, verbose, stdout, stderr)
	}
	// Detect first-run-without-add: the project has frontend
	// sources under islands.src/ but no nexus.lock yet. Print a
	// specific suggestion (with per-import nexus add commands)
	// instead of falling through to the vite path, which would
	// surface as "--frontend-cmd is empty" and leave the user
	// guessing what to do.
	if hint := maybeMissingLockfileHint(dir); hint != "" {
		fmt.Fprintf(stderr, "%s%s%s\n", ansiYellow, hint, ansiReset)
		return nil
	}
	return startViteWatcher(ctx, dir, cmdline, verbose, stdout, stderr, frontendURLCh)
}

// maybeMissingLockfileHint scans dir (and one parent up, matching
// depsModeAvailable's walk shape) for the new-pipeline shape —
// islands.src/ with entries. Returns a helpful message when the
// pipeline IS in use but nexus.lock hasn't been populated yet;
// returns "" otherwise so the caller can fall through to the
// legacy vite path.
//
// Single-level upward walk because the new pipeline always has
// islands.src/ at the project root. Deeper walks would mistake
// nested-project layouts for the user's project.
func maybeMissingLockfileHint(dir string) string {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for _, candidate := range []string{cur, filepath.Dir(cur)} {
		srcDir := filepath.Join(candidate, "islands.src")
		if info, err := os.Stat(srcDir); err != nil || !info.IsDir() {
			continue
		}
		entries, err := collectFrontendEntries(srcDir)
		if err != nil || len(entries) == 0 {
			continue
		}
		return "nexus dev: " + formatMissingLockfileError(srcDir, entries)
	}
	return ""
}

// depsModeAvailable reports whether dir (or any directory between
// dir and cwd, inclusive) holds a nexus.lock. Walking upward lets
// the auto-detection in dev.go work even when the user passed a
// frontend subdir like ./web — the lockfile usually sits one
// level up at the project root.
//
// Bounded walk: we stop at the first nexus.lock OR at the
// filesystem root, whichever comes first. No depth cap because
// the path is bounded by the OS already.
func depsModeAvailable(dir string) bool {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	for {
		if _, err := os.Stat(filepath.Join(cur, "nexus.lock")); err == nil {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// startBundlerWatcher drives our esbuild-based bundler in watch
// mode against <project-root>/islands.src, writing bundles into
// <project-root>/islands as the user edits files. Lifecycle:
//
//  1. Walk upward from dir to find nexus.lock; treat that
//     directory as the project root.
//  2. Load lockfile + open the shared cache via NEXUS_CACHE env
//     (default ~/.nexus/cache).
//  3. Construct the resolver plugin that translates bare imports
//     into cached blob paths.
//  4. Enumerate islands.src/*.{ts,tsx,jsx,js} as entries; reject
//     .vue with a clear message (Vue SFC compile is gated in
//     v0.1, see frontend/deps/sfc/vue/bootstrap_test.go).
//  5. bundler.Build(Watch: true, OnRebuild: reporter). First
//     build runs synchronously; subsequent edits fire the
//     callback asynchronously inside esbuild's watcher loop.
//  6. Goroutine watches ctx and disposes the BuildContext on
//     shutdown so esbuild closes its file watches cleanly.
//
// Verbose has a meaningful effect here: when off, the reporter
// only prints when something USEFUL changes (bundle output size
// drifted, errors appeared, errors cleared). When on, every
// rebuild prints a one-line summary even when nothing changed
// in the output bytes.
//
// No URL ever lands on frontendURLCh — bundler mode produces
// on-disk files served by the Go binary, not a separate HTTP
// dev server. dev.go's waitAndOpen handles the missing-URL case
// via its grace-window timeout fallback to the gin URL.
func startBundlerWatcher(ctx context.Context, dir string, verbose bool, stdout, stderr io.Writer) error {
	root, err := findProjectRoot(dir)
	if err != nil {
		return fmt.Errorf("frontend watcher: %w", err)
	}

	srcDir := filepath.Join(root, "islands.src")
	if _, err := os.Stat(srcDir); errors.Is(err, fs.ErrNotExist) {
		// Project has a lockfile but no islands.src — nothing to
		// watch. Print a one-liner so the user understands why
		// no [web] output is appearing.
		fmt.Fprintf(stdout, "%s●%s frontend watcher: no islands.src — skipping\n", ansiCyan, ansiReset)
		return nil
	} else if err != nil {
		return fmt.Errorf("frontend watcher: stat islands.src: %w", err)
	}

	entries, err := collectFrontendEntries(srcDir)
	if err != nil {
		return fmt.Errorf("frontend watcher: %w", err)
	}
	if len(entries) == 0 {
		fmt.Fprintf(stdout, "%s●%s frontend watcher: islands.src is empty — skipping\n", ansiCyan, ansiReset)
		return nil
	}
	for _, e := range entries {
		if strings.HasSuffix(e, ".vue") {
			return fmt.Errorf("frontend watcher: %s is a .vue source — v0.1 doesn't yet "+
				"compile Vue SFC in watch mode; pre-compile to .js or use the legacy vite path", e)
		}
	}

	lf, err := lockfile.Load(filepath.Join(root, lockfile.Filename))
	if err != nil {
		return fmt.Errorf("frontend watcher: load lockfile: %w", err)
	}

	cacheRoot := os.Getenv("NEXUS_CACHE")
	if cacheRoot == "" {
		cacheRoot = store.DefaultRoot()
	}
	st, err := store.New(cacheRoot)
	if err != nil {
		return fmt.Errorf("frontend watcher: open cache %s: %w", cacheRoot, err)
	}

	plugin, err := resolver.New(resolver.Options{Lockfile: lf, Store: st})
	if err != nil {
		return fmt.Errorf("frontend watcher: build resolver: %w", err)
	}

	outDir := filepath.Join(root, "islands")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("frontend watcher: mkdir %s: %w", outDir, err)
	}

	tag := fmt.Sprintf("%s[web]%s ", ansiCyan, ansiReset)
	reporter := newBundlerReporter(stdout, stderr, tag, verbose)

	b := bundler.New()
	b.AddPlugin(plugin)
	res, err := b.Build(bundler.Options{
		Entries:   entries,
		OutDir:    outDir,
		Lockfile:  lf,
		Store:     st,
		Minify:    false, // dev: readable output > smaller bytes
		Watch:     true,
		OnRebuild: reporter.report,
	})
	if err != nil {
		return fmt.Errorf("frontend watcher: %w", err)
	}
	// Initial build's result also flows through the reporter so
	// the first banner line + any startup errors print the same
	// way subsequent rebuilds do.
	reporter.report(res.BuildResult)

	// Banner: print after the initial report so the order reads
	// "bundled N entries" then "watching — Ctrl-C to stop".
	fmt.Fprintf(stdout, "%s●%s frontend watcher: watching %s (%d %s) — esbuild incremental\n",
		ansiCyan, ansiReset, srcDir, len(entries), pluralize("entry", len(entries)))

	// Tear down esbuild's watcher on ctx cancel. Dispose closes
	// the file watches + releases the build context; without it,
	// the watcher goroutines outlive nexus dev and leak fds.
	if res.Ctx != nil {
		go func() {
			<-ctx.Done()
			res.Ctx.Dispose()
		}()
	}
	return nil
}

// findProjectRoot walks upward from dir until it finds nexus.lock
// and returns that directory. Returns an error when no lockfile
// is found before the filesystem root; callers should already
// have checked depsModeAvailable so this is mostly a defensive
// guard.
func findProjectRoot(dir string) (string, error) {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(cur, "nexus.lock")); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("no nexus.lock found at or above %s", dir)
		}
		cur = parent
	}
}

// bundlerReporter prints rebuild summaries to stdout/stderr with
// the [web] tag. Tracks the previous build's output-file sizes so
// it can emit a delta line ("counter.js  4.2 KB → 4.5 KB") on
// each rebuild rather than re-listing every file every time.
//
// Verbose mode prints a one-line "rebuilt in Xms" even when the
// output didn't change. Default mode suppresses no-change
// rebuilds entirely (matches the quiet vite-mode behavior).
type bundlerReporter struct {
	stdout, stderr io.Writer
	tag            string
	verbose        bool

	mu       sync.Mutex
	prev     map[string]int64 // outfile → byte size
	hadFirst bool
	prevErrs int
	start    time.Time
}

func newBundlerReporter(stdout, stderr io.Writer, tag string, verbose bool) *bundlerReporter {
	return &bundlerReporter{
		stdout:  stdout,
		stderr:  stderr,
		tag:     tag,
		verbose: verbose,
		prev:    map[string]int64{},
		start:   time.Now(),
	}
}

func (r *bundlerReporter) report(res api.BuildResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Errors always print to stderr verbatim — these are the most
	// actionable lines the user sees, so they should never be
	// dedup'd. Warnings get the same treatment.
	for _, e := range res.Errors {
		r.printDiag(r.stderr, "✗", ansiRed, e)
	}
	for _, w := range res.Warnings {
		r.printDiag(r.stderr, "⚠", ansiYellow, w)
	}

	// Build a fresh sizes map keyed by basename, paired with full
	// path for diagnostics. esbuild emits absolute paths in
	// OutputFiles; the basename is what the user thinks of as
	// "Counter.js".
	curr := map[string]int64{}
	for _, f := range res.OutputFiles {
		curr[filepath.Base(f.Path)] = int64(len(f.Contents))
	}

	// Compute added / changed / removed sets relative to prev.
	type change struct{ name, kind string }
	var changes []change
	for name, size := range curr {
		prev, ok := r.prev[name]
		switch {
		case !ok:
			changes = append(changes, change{name, fmt.Sprintf("+ %s  %s", name, humanBytes(size))})
		case prev != size:
			changes = append(changes, change{name, fmt.Sprintf("~ %s  %s → %s", name, humanBytes(prev), humanBytes(size))})
		}
	}
	for name := range r.prev {
		if _, ok := curr[name]; !ok {
			changes = append(changes, change{name, fmt.Sprintf("- %s", name)})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].name < changes[j].name })

	// First report after the watcher starts: print every entry as
	// an "+ N" line so the user sees the initial bundle shape.
	if !r.hadFirst {
		r.hadFirst = true
		if len(res.Errors) == 0 && len(curr) > 0 {
			for _, c := range changes {
				fmt.Fprintf(r.stdout, "%s  %s%s%s\n", r.tag, ansiGreen, c.kind, ansiReset)
			}
		}
		r.prev = curr
		r.prevErrs = len(res.Errors)
		return
	}

	switch {
	case len(changes) > 0:
		for _, c := range changes {
			color := ansiYellow
			if strings.HasPrefix(c.kind, "+ ") {
				color = ansiGreen
			} else if strings.HasPrefix(c.kind, "- ") {
				color = ansiRed
			}
			fmt.Fprintf(r.stdout, "%s  %s%s%s\n", r.tag, color, c.kind, ansiReset)
		}
	case r.prevErrs > 0 && len(res.Errors) == 0:
		// Errors cleared since last build — emit a recovery line
		// so the user knows the red they saw a moment ago is gone.
		fmt.Fprintf(r.stdout, "%s  %s● errors resolved%s\n", r.tag, ansiGreen, ansiReset)
	case r.verbose:
		// Verbose: print every rebuild even if no output bytes
		// changed (useful for confirming the watcher actually
		// fired in response to a save).
		fmt.Fprintf(r.stdout, "%s  %s● rebuilt — no output changes%s\n", r.tag, ansiDim, ansiReset)
	}

	r.prev = curr
	r.prevErrs = len(res.Errors)
}

// printDiag renders one esbuild diagnostic with file:line:col +
// the message text. The first line in the build with errors gets
// a fresh blank line before it so the diagnostic block stands
// apart from prior output.
func (r *bundlerReporter) printDiag(w io.Writer, marker, color string, m api.Message) {
	loc := ""
	if m.Location != nil {
		loc = fmt.Sprintf(" %s:%d:%d", m.Location.File, m.Location.Line, m.Location.Column)
	}
	fmt.Fprintf(w, "%s%s%s%s %s%s\n", r.tag, color, marker, loc, m.Text, ansiReset)
	if m.Location != nil && m.Location.LineText != "" {
		fmt.Fprintf(w, "%s     %s%s%s\n", r.tag, ansiDim, m.Location.LineText, ansiReset)
	}
}

// humanBytes renders b as a friendly size (e.g. "4.2 KB"). Used in
// the reporter's delta lines so the user sees "Counter.js
// 4.2 KB → 4.5 KB" instead of raw byte counts.
func humanBytes(b int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
	)
	switch {
	case b >= mib:
		return fmt.Sprintf("%.1f MB", float64(b)/mib)
	case b >= kib:
		return fmt.Sprintf("%.1f KB", float64(b)/kib)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// startViteWatcher is the legacy npm/vite shellout path, preserved
// for projects that haven't migrated to nexus add / nexus build.
// Verbatim port of the pre-deps-system implementation: spawns
// `sh -c <cmdline>` in dir, prefixes [web], sniffs Local: URLs,
// dedupes identical vite rebuild blocks, tears down the whole
// process group on ctx cancel.
func startViteWatcher(ctx context.Context, dir, cmdline string, verbose bool, stdout, stderr io.Writer, frontendURLCh chan<- string) error {
	cmdline = strings.TrimSpace(cmdline)
	if cmdline == "" {
		return fmt.Errorf("--frontend-cmd is empty")
	}
	// Honor the user's shell so quoted args + npm scripts that
	// fork their own child processes work without a Go-side parser.
	// exec.Command (not CommandContext) is deliberate: the default
	// CommandContext cancel path SIGKILLs only the immediate child
	// (`sh -c`), which orphans `npm run dev` → vite/node and leaks
	// the dev-server port. We watch ctx.Done() below and tear down
	// the whole process group via killProcessGroup.
	cmd := exec.Command("sh", "-c", cmdline)
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
	// Sniff every line for vite's "Local: http://..." marker —
	// regardless of mode, regardless of filter — so the dev-server
	// auto-open path can race against it without caring whether
	// we're in bundle mode (where the line never arrives).
	maybeReportURL := func(line string) {
		if frontendURLCh == nil {
			return
		}
		if m := viteURLRE.FindStringSubmatch(line); m != nil {
			select {
			case frontendURLCh <- m[1]:
			default: // already reported once; the channel is buffered=1
			}
		}
	}
	var wg sync.WaitGroup
	stderrPump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			maybeReportURL(line)
			fmt.Fprintf(dst, "%s%s\n", tag, line)
		}
	}
	stdoutPump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		if verbose {
			for scanner.Scan() {
				line := scanner.Text()
				maybeReportURL(line)
				fmt.Fprintf(dst, "%s%s\n", tag, line)
			}
			return
		}
		f := newBuildBlockFilter(dst, tag)
		for scanner.Scan() {
			line := scanner.Text()
			maybeReportURL(line)
			f.line(line)
		}
		f.flush()
	}
	wg.Add(2)
	go stdoutPump(stdoutPipe, stdout)
	go stderrPump(stderrPipe, stderr)

	// Tear the whole child group down on ctx cancel — SIGTERM
	// first so vite/npm get a chance to release their port, then
	// SIGKILL after a grace window if anything's still alive.
	// Without this, `sh -c` dies but the grandchild node/vite
	// keep the dev-server port bound until the user kills them.
	pid := cmd.Process.Pid
	go func() {
		<-ctx.Done()
		_ = killProcessGroup(pid, syscall.SIGTERM)
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-timer.C:
			_ = killProcessGroup(pid, syscall.SIGKILL)
		}
	}()

	go func() {
		wg.Wait()
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(stderr, "%sfrontend watcher exited: %v%s\n", ansiYellow, err, ansiReset)
		}
	}()

	fmt.Fprintf(stdout, "%s●%s frontend watcher: %q in %s\n", ansiCyan, ansiReset, cmdline, dir)
	if !verbose {
		fmt.Fprintf(stdout, "  %s(identical rebuilds collapsed — pass --verbose for the raw stream)%s\n", ansiDim, ansiReset)
	}
	return nil
}

// Patterns that bracket a vite/rollup rebuild cycle and recognize
// the asset summary lines inside it.
var (
	buildStartedRE = regexp.MustCompile(`build started\.{3}`)
	buildEndedRE   = regexp.MustCompile(`built in ([\d.]+\s?m?s)\.?$`)
	// assetParseRE pulls the path + size + gzip out of a vite
	// asset summary line:
	//   "dist/assets/index-CgaP6gEu.js   389.02 kB │ gzip: 109.45 kB"
	assetParseRE = regexp.MustCompile(`^\s*(dist/\S+)\s+([\d.]+)\s*kB\s*│\s*gzip:\s*([\d.]+)\s*kB`)
	// hashStripRE peels vite's 8-char content hash off a filename
	// so two builds that only differ by hash-shuffling (no size
	// change) collapse to the same logical key. Vite hashes are
	// always 8 chars from [A-Za-z0-9_-] immediately before the
	// extension; the dash-prefix is stable across versions.
	//   LoginView-Df8jaTme.js                  → LoginView.js
	//   vendor-nuxt-ui-CFRnWYHU.js             → vendor-nuxt-ui.js
	//   DualListBox.vue_vue_type_..._lang-C-Rtz0G-.js
	//                                          → DualListBox.vue_vue_type_..._lang.js
	hashStripRE = regexp.MustCompile(`-[A-Za-z0-9_-]{8}(\.[a-z]+)$`)
)

// assetEntry captures the comparison key for a single asset: its
// logical (un-hashed) path and its reported sizes. Two entries are
// "the same" iff all three fields match — that's what tells us
// vite produced an identical chunk regardless of hash shuffling
// from module-id reordering.
type assetEntry struct {
	logical string
	size    string
	gzip    string
	line    string // original printed line, for emit
}

// buildBlockFilter compares the asset summary table emitted at the
// end of every `vite build --watch` cycle against the previous
// cycle's table. Asset lines that appear in BOTH tables (filename
// AND reported sizes byte-identical) are suppressed — they didn't
// change, the user doesn't need to see them. Asset lines that
// appear only in the new cycle (added or content-hash-changed) are
// emitted with a green marker. Lines that disappeared are emitted
// with a red marker.
//
// The "build started…" header and the "built in Xms" footer are
// summarized into one line:
//   - no asset changes (the auto-import-plugin self-rebuild loop
//     symptom): "● rebuild · no changes (Xms)"
//   - some changes:                            "● 3 changed · 38 unchanged (Xms)"
//
// First cycle of the session emits the full asset table verbatim
// so the user sees the initial bundle shape. Subsequent cycles
// emit only the delta.
type buildBlockFilter struct {
	dst        io.Writer
	tag        string
	inBuild    bool
	buffer     []string
	prevAssets map[string]assetEntry
	hadFirst   bool
}

func newBuildBlockFilter(dst io.Writer, tag string) *buildBlockFilter {
	return &buildBlockFilter{dst: dst, tag: tag}
}

func (f *buildBlockFilter) line(s string) {
	switch {
	case buildStartedRE.MatchString(s):
		if f.inBuild {
			// Cycle never closed — flush the partial buffer so we
			// don't silently drop output.
			f.flushPartial()
		}
		f.inBuild = true
		f.buffer = []string{s}
	case f.inBuild:
		f.buffer = append(f.buffer, s)
		if buildEndedRE.MatchString(s) {
			f.endCycle()
		}
	default:
		// Suppress blank-line spacers vite emits between cycles —
		// without them, no-change rebuilds produce zero output.
		if strings.TrimSpace(s) == "" {
			return
		}
		fmt.Fprintf(f.dst, "%s%s\n", f.tag, s)
	}
}

func (f *buildBlockFilter) endCycle() {
	currAssets := extractAssets(f.buffer)
	duration := extractDuration(f.buffer)

	if !f.hadFirst {
		// First cycle: emit the buffer verbatim so users see the
		// initial bundle shape. Stash assets for next-cycle diff.
		for _, l := range f.buffer {
			fmt.Fprintf(f.dst, "%s%s\n", f.tag, l)
		}
		f.prevAssets = currAssets
		f.hadFirst = true
		f.inBuild = false
		f.buffer = nil
		return
	}

	type sizeChange struct{ logical, oldSize, newSize, oldGzip, newGzip string }
	var added, removed []assetEntry
	var changed []sizeChange
	unchanged := 0

	for k, curr := range currAssets {
		prev, ok := f.prevAssets[k]
		if !ok {
			added = append(added, curr)
			continue
		}
		if prev.size == curr.size && prev.gzip == curr.gzip {
			unchanged++
			continue
		}
		changed = append(changed, sizeChange{
			logical: k, oldSize: prev.size, newSize: curr.size,
			oldGzip: prev.gzip, newGzip: curr.gzip,
		})
	}
	for k, prev := range f.prevAssets {
		if _, ok := currAssets[k]; !ok {
			removed = append(removed, prev)
		}
	}

	if len(added)+len(removed)+len(changed) == 0 {
		// No-change rebuild — emit nothing. Vite/auto-import-style
		// loops can fire dozens of these per minute; even a single
		// summary line per cycle floods the terminal. Silence is
		// the right default; --verbose still streams the raw
		// output for users who want to see every cycle.
		_ = duration
	} else {
		//fmt.Fprintf(f.dst, "%s● %d changed · %d unchanged (%s)\n",
		//	f.tag, len(added)+len(removed)+len(changed), unchanged, duration)
		for _, e := range removed {
			fmt.Fprintf(f.dst, "%s  %s- %s%s\n", f.tag, ansiRed, e.logical, ansiReset)
		}
		for _, c := range changed {
			fmt.Fprintf(f.dst, "%s  %s~ %s  %s → %s kB │ gzip %s → %s kB%s\n",
				f.tag, ansiYellow, c.logical, c.oldSize, c.newSize, c.oldGzip, c.newGzip, ansiReset)
		}
		for _, e := range added {
			fmt.Fprintf(f.dst, "%s  %s+ %s  %s kB │ gzip %s kB%s\n",
				f.tag, ansiGreen, e.logical, e.size, e.gzip, ansiReset)
		}
	}

	f.prevAssets = currAssets
	f.inBuild = false
	f.buffer = nil
}

func (f *buildBlockFilter) flushPartial() {
	for _, l := range f.buffer {
		fmt.Fprintf(f.dst, "%s%s\n", f.tag, l)
	}
	f.buffer = nil
	f.inBuild = false
}

// flush drains any pending buffer when the upstream stream closes.
func (f *buildBlockFilter) flush() {
	if f.inBuild {
		f.flushPartial()
	}
}

// extractAssets parses every "dist/<path>  size kB │ gzip: gzip
// kB" line out of buffer and returns logical-name → assetEntry.
// "Logical name" is the path with vite's 8-char content hash
// stripped, so two builds that only shuffled hashes (because
// module IDs reordered) produce the same key. Sizes drive the
// "did this asset really change?" decision in endCycle.
func extractAssets(buffer []string) map[string]assetEntry {
	out := make(map[string]assetEntry)
	for _, l := range buffer {
		m := assetParseRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		path, size, gzip := m[1], m[2], m[3]
		logical := hashStripRE.ReplaceAllString(path, "$1")
		out[logical] = assetEntry{logical: logical, size: size, gzip: gzip, line: l}
	}
	return out
}

// extractDuration returns the "Xms" suffix from the cycle's
// "built in" line, or "?" when it wasn't found.
func extractDuration(buffer []string) string {
	for _, l := range buffer {
		if m := buildEndedRE.FindStringSubmatch(l); m != nil {
			return m[1]
		}
	}
	return "?"
}
