package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/fsnotify/fsnotify"

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
		srcDir := filepath.Join(candidate, islandsSrcName())
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

	srcName := islandsSrcName()
	srcDir := filepath.Join(root, srcName)
	// Wait-for-entries loop. A fresh project commonly has the
	// folder present but empty (or even absent), or the operator
	// runs `nexus dev` before dropping in the first entry file.
	// Rather than skip-and-exit (which forced a restart of nexus
	// dev whenever the operator added their first entry), block
	// here with an fsnotify watch on srcDir's parent (so we catch
	// the dir's creation too) until at least one matching entry
	// appears. ctx cancellation breaks the loop cleanly so a
	// Ctrl-C during the wait doesn't hang.
	actualSrcDir, entries, err := waitForFrontendEntries(ctx, srcDir, srcName, stdout)
	if err != nil {
		return err
	}
	if entries == nil {
		// ctx cancelled while waiting — quiet exit.
		return nil
	}
	if actualSrcDir != srcDir {
		// Auto-descended into src/ (or app/ / client/). Switch
		// srcDir for every downstream check + log path so the
		// watch tree, vue scan, banner all reflect the resolved
		// dir instead of the operator-requested one.
		rel, _ := filepath.Rel(root, actualSrcDir)
		if rel == "" {
			rel = actualSrcDir
		}
		fmt.Fprintf(stdout, "%s●%s frontend watcher: auto-detected entries under %s (no top-level entries in %s)\n",
			ansiCyan, ansiReset, rel, srcName)
		srcDir = actualSrcDir
	}
	// Walk islands.src/ recursively — a .vue file colocated under
	// any subdirectory still needs the SFC compiler, even if no
	// top-level entry is .vue. Treating only entry extensions as
	// the signal would miss the common case of main.ts importing
	// ./components/Foo.vue.
	hasVue, err := hasVueSources(srcDir)
	if err != nil {
		return fmt.Errorf("frontend watcher: scan vue sources: %w", err)
	}
	if hasVue && vueCompilerHook == nil {
		return errors.New("frontend watcher: .vue sources detected but no SFC compiler is wired — " +
			"you built with `-tags vue` (native CGo backend) without cgo. Either set CGO_ENABLED=1, " +
			"or drop `-tags vue` to use the default WASM backend (no cgo needed)")
	}

	lockPath := filepath.Join(root, lockfile.Filename)
	lf, err := lockfile.Load(lockPath)
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

	plugin, err := resolver.New(resolver.Options{
		Lockfile:      lf,
		Store:         st,
		FetchOnDemand: makeOnDemandFetch(lf, st, lockPath, stdout),
	})
	if err != nil {
		return fmt.Errorf("frontend watcher: build resolver: %w", err)
	}

	outDir := filepath.Join(root, islandsOutName())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("frontend watcher: mkdir %s: %w", outDir, err)
	}

	// Mirror public/* into the output dir so HTML-referenced
	// assets (favicons, PWA icons, manifest.json, robots.txt)
	// resolve at the same paths their <link>/<meta> tags claim.
	// esbuild handles code-imported assets via the file loader;
	// public/ is the parallel convention for assets the HTML
	// hard-codes without an import statement.
	//
	// Two candidate locations: the user might keep public/
	// alongside the islands.src root (Vite convention) OR
	// inside the entries' subdir. First match wins.
	for _, candidate := range []string{
		filepath.Join(root, islandsSrcName(), "public"),
		filepath.Join(srcDir, "public"),
	} {
		n, perr := bundler.CopyPublicDir(candidate, outDir)
		if perr != nil {
			fmt.Fprintf(stderr, "%s●%s frontend watcher: public/ copy: %v\n", ansiYellow, ansiReset, perr)
			break
		}
		if n > 0 {
			fmt.Fprintf(stdout, "%s[web]%s public: copied %d file(s) from %s\n", ansiCyan, ansiReset, n, candidate)
			break
		}
	}

	tag := fmt.Sprintf("%s[web]%s ", ansiCyan, ansiReset)
	reporter := newBundlerReporter(stdout, stderr, tag, verbose)

	b := bundler.New()
	b.AddPlugin(plugin)

	// Vue SFC plugin is registered the same way frontendBuild does
	// it: the build-tagged hook returns a (close, plugin) pair so
	// the QuickJS compiler's lifetime tracks the watcher's. The
	// closer fires from the ctx-cancellation goroutine below so a
	// Ctrl-C / shutdown disposes the JS runtime cleanly.
	var closeVue func()
	if hasVue && vueCompilerHook != nil {
		cv, vuePlugin, err := vueCompilerHook(lf, st)
		if err != nil {
			return fmt.Errorf("frontend watcher: vue compiler: %w", err)
		}
		closeVue = cv
		b.AddPlugin(vuePlugin)
	}

	// Sass plugin is registered unconditionally — the .scss
	// resolver only matches when the operator actually imports
	// one. When sass isn't on PATH the plugin surfaces a clear
	// install-suggestion error at compile time, which is more
	// useful than esbuild's silent "no loader configured" miss.
	b.AddPlugin(bundler.NewSassPlugin())
	if bundler.SassAvailable() {
		fmt.Fprintf(stdout, "%s[web]%s scss via system sass\n", ansiCyan, ansiReset)
	}

	// Tailwind plugin — same shape as sass: only fires when a
	// CSS file actually contains @tailwind / @apply directives,
	// otherwise transparent. Surfaces a clear install-suggestion
	// error if directives are present but tailwindcss is
	// missing from PATH.
	b.AddPlugin(bundler.NewTailwindPlugin())
	if bundler.TailwindAvailable() {
		fmt.Fprintf(stdout, "%s[web]%s tailwind via system tailwindcss\n", ansiCyan, ansiReset)
	}

	// import.meta.glob plugin — rewrites Vite-style glob calls
	// into object literals at bundle time. No-op on files
	// that don't use it; Vue Router auto-discovery + similar
	// patterns work out of the box.
	b.AddPlugin(bundler.NewImportMetaGlobPlugin())

	// ?raw / ?url / ?inline query suffix plugin — Vite-style
	// import attribute queries that change how a file is
	// loaded (inline as string, emit as URL, base64 data URL).
	// No-op on imports without recognized suffixes.
	b.AddPlugin(bundler.NewQuerySuffixPlugin())

	// ?worker plugin — bundles workers as separate entries +
	// returns a Worker constructor class. Registered LAST so
	// the sub-build inherits every other plugin (resolver,
	// sass, tailwind, globs, env, etc.) — b.Plugins read
	// here is a snapshot taken before we add the worker
	// itself, so no recursion.
	b.AddPlugin(bundler.NewWorkerPlugin(bundler.WorkerPluginOptions{
		OutDir:        outDir,
		PublicPath:    os.Getenv("NEXUS_PUBLIC_PATH"),
		NestedPlugins: append([]api.Plugin(nil), b.Plugins...),
	}))

	// Initial build runs SYNCHRONOUSLY here — esbuild's Watch:true
	// completes the first pass before returning, so by the time
	// the function exits the bundle is already on disk under
	// outDir. The watcher then fires OnRebuild for every
	// subsequent file change. The Go child process is launched
	// AFTER this returns (see startFrontendWatcher → dev.go
	// orchestration), so on first boot the binary's //go:embed
	// can pick up the freshly-bundled files.
	fmt.Fprintf(stdout, "%s●%s frontend watcher: initial build of %d %s from %s…\n",
		ansiCyan, ansiReset, len(entries), pluralize("entry", len(entries)), srcDir)
	// Wrap the rebuild reporter so every rebuild — including
	// the initial one — also refreshes the transformed
	// index.html in outDir. Without that, edits to the source
	// `index.html` (or to the entry list) wouldn't propagate
	// to the served shell during a session.
	onRebuild := func(br api.BuildResult) {
		reporter.report(br)
		// emitIndexHTML is best-effort; a fmt error shouldn't
		// kill the watcher. devMode=true so the dev-reload
		// shim is injected — that's what the browser uses to
		// auto-refresh when this rebuild lands.
		_ = emitIndexHTML(srcDir, outDir, br.OutputFiles, io.Discard, true)
	}
	tsconfig := findProjectTSConfig(root)
	if tsconfig != "" {
		fmt.Fprintf(stdout, "%s[web]%s tsconfig: %s\n", ansiCyan, ansiReset, tsconfig)
	}
	viteEnv, envErr := loadViteEnv(root, "development", stdout)
	if envErr != nil {
		fmt.Fprintf(stderr, "%s●%s frontend watcher: .env load: %v\n", ansiYellow, ansiReset, envErr)
	}
	res, err := b.Build(bundler.Options{
		Entries:    entries,
		OutDir:     outDir,
		Lockfile:   lf,
		Store:      st,
		TSConfig:   tsconfig,
		Env:        viteEnv,
		Mode:       "development",
		PublicPath: os.Getenv("NEXUS_PUBLIC_PATH"),
		Minify:     false, // dev: readable output > smaller bytes
		Splitting:  true,
		Watch:      true,
		OnRebuild:  onRebuild,
	})
	if err != nil {
		return fmt.Errorf("frontend watcher: %w", err)
	}
	// Initial build's result also flows through the reporter so
	// the first banner line + any startup errors print the same
	// way subsequent rebuilds do.
	reporter.report(res.BuildResult)
	if err := emitIndexHTML(srcDir, outDir, res.OutputFiles, stdout, true); err != nil {
		fmt.Fprintf(stderr, "%s●%s frontend watcher: index.html emit: %v\n", ansiYellow, ansiReset, err)
	}

	// Banner: print after the initial report so the order reads
	// "bundled N entries" then "watching — Ctrl-C to stop".
	fmt.Fprintf(stdout, "%s●%s frontend watcher: initial build done — watching %s for changes (esbuild incremental)\n",
		ansiCyan, ansiReset, srcDir)

	// Tear down esbuild's watcher on ctx cancel. Dispose closes
	// the file watches + releases the build context; without it,
	// the watcher goroutines outlive nexus dev and leak fds.
	// Same goroutine also releases the Vue compiler's QuickJS
	// runtime if one was bootstrapped, so a shutdown doesn't
	// strand the worker.
	if res.Ctx != nil || closeVue != nil {
		go func() {
			<-ctx.Done()
			if res.Ctx != nil {
				res.Ctx.Dispose()
			}
			if closeVue != nil {
				closeVue()
			}
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
	// Warnings from cached third-party deps (esbuild's nexus-deps
	// namespace) aren't actionable by the user — they live inside
	// vendored/minified blobs like pdfmake — so they're noise in the
	// dev console. Hide them by default, keeping warnings about the
	// user's own source, and print a one-line tally so they're not
	// invisible. --verbose shows everything.
	var hiddenDepWarns int
	for _, w := range res.Warnings {
		if !r.verbose && isVendorDiag(w) {
			hiddenDepWarns++
			continue
		}
		r.printDiag(r.stderr, "⚠", ansiYellow, w)
	}
	if hiddenDepWarns > 0 {
		fmt.Fprintf(r.stderr, "%s%s⚠ %d %s from cached dependencies hidden (run with --verbose to show)%s\n",
			r.tag, ansiDim, hiddenDepWarns, pluralize("warning", hiddenDepWarns), ansiReset)
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
// isVendorDiag reports whether a diagnostic originates from a cached
// third-party dependency rather than the user's own source. The
// resolver loads those blobs in esbuild's nexus-deps namespace, so
// esbuild stamps their location file as "nexus-deps:<url>". Such
// warnings (e.g. duplicate object keys in a minified vendor bundle)
// aren't fixable from user code, so the reporter hides them by default.
func isVendorDiag(m api.Message) bool {
	return m.Location != nil && strings.HasPrefix(m.Location.File, resolver.Namespace+":")
}

func (r *bundlerReporter) printDiag(w io.Writer, marker, color string, m api.Message) {
	loc := ""
	if m.Location != nil {
		loc = fmt.Sprintf(" %s:%d:%d", m.Location.File, m.Location.Line, m.Location.Column)
	}
	fmt.Fprintf(w, "%s%s%s%s %s%s\n", r.tag, color, marker, loc, m.Text, ansiReset)
	if m.Location != nil && m.Location.LineText != "" {
		// Suppress source-context dumps for minified third-party
		// code. esbuild reports LineText verbatim from the source
		// — for minified bundles where the entire file is on one
		// line (pdfmake.mjs being a frequent offender at ~200KB
		// of body on a single line), the "context" floods the
		// terminal with junk that masks every other error.
		lineText := m.Location.LineText
		const maxLineTextLen = 200
		if len(lineText) > maxLineTextLen {
			lineText = lineText[:maxLineTextLen] + "…"
		}
		fmt.Fprintf(w, "%s     %s%s%s\n", r.tag, ansiDim, lineText, ansiReset)
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
	// #nosec G204 -- CLI dev helper, cmdline is operator-supplied via --frontend-cmd
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

// waitForFrontendEntries blocks until srcDir contains at least one
// entry file (.ts / .tsx / .jsx / .js at the top level). Handles
// three starting states:
//
//   - srcDir doesn't exist: watches the parent for the dir's
//     creation, then falls through to the empty-but-exists path.
//   - srcDir exists but is empty: watches srcDir for any matching
//     file Create / Rename / Write event.
//   - srcDir has entries: returns them immediately, no fsnotify
//     setup at all.
//
// Returns (entries, nil) on success, (nil, nil) on ctx
// cancellation (caller exits quietly), or (nil, err) on a
// genuine watcher / I/O failure. The two-channel error vs.
// cancel split keeps the caller's main loop simple.
//
// Why we don't just skip + tell the operator to restart: a fresh
// project commonly runs `nexus dev` BEFORE the first entry file
// lands. Skipping there meant the operator had to Ctrl-C + relaunch
// after adding the file — friction that boils down to a missing
// fsnotify on the source folder.
func waitForFrontendEntries(ctx context.Context, srcDir, srcName string, stdout io.Writer) (string, []string, error) {
	// First peek: if the dir already has entries (or a conventional
	// subdir does — Vite layouts), skip every watcher setup.
	// Fast-path the steady-state case so the cost of this helper
	// is one os.ReadDir.
	if actualDir, entries, err := peekForEntries(srcDir); err != nil {
		return srcDir, nil, err
	} else if len(entries) > 0 {
		return actualDir, entries, nil
	}

	// State 1: srcDir is missing → watch the parent for its
	// creation. We can't watch a non-existent directory directly.
	if _, err := os.Stat(srcDir); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "%s●%s frontend watcher: %s not present — waiting for it to be created (Ctrl-C to stop)\n",
			ansiCyan, ansiReset, srcName)
		if err := waitForDirCreate(ctx, srcDir); err != nil {
			return srcDir, nil, err
		}
		if ctx.Err() != nil {
			return srcDir, nil, nil
		}
		// dir exists now; re-check + fall through to State 2 if
		// it's still empty.
		if actualDir, entries, err := peekForEntries(srcDir); err != nil {
			return srcDir, nil, err
		} else if len(entries) > 0 {
			return actualDir, entries, nil
		}
	} else if err != nil {
		return srcDir, nil, fmt.Errorf("frontend watcher: stat %s: %w", srcName, err)
	}

	// State 2: srcDir exists but has no matching entry → watch
	// for the first one to land. Differentiate three sub-cases
	// so the operator knows whether they need to drop a file in
	// (truly empty), or add a bootstrap entry next to their
	// existing components / .vue files (the common case for
	// fresh Vue/React setups where they have `App.vue` /
	// `App.tsx` but no `main.ts` yet).
	switch describeEmptyState(srcDir) {
	case emptyStateNoFiles:
		fmt.Fprintf(stdout, "%s●%s frontend watcher: %s is empty — waiting for first entry file (Ctrl-C to stop)\n",
			ansiCyan, ansiReset, srcName)
	case emptyStateOnlyVue:
		fmt.Fprintf(stdout, "%s●%s frontend watcher: %s has .vue files but no top-level .ts/.tsx entry — waiting\n",
			ansiCyan, ansiReset, srcName)
		fmt.Fprintf(stdout, "%s   create a bootstrap file (e.g. main.ts) that imports your components, like:%s\n",
			ansiCyan, ansiReset)
		fmt.Fprintf(stdout, "      import { createApp } from 'vue'\n")
		fmt.Fprintf(stdout, "      import App from './App.vue'\n")
		fmt.Fprintf(stdout, "      createApp(App).mount('#app')\n")
	case emptyStateNoTopLevel:
		// Common gotcha: Vite-style projects keep their bootstrap
		// under `src/`, so the operator drops their existing
		// repo under islands.src/ and ends up with the entry one
		// directory too deep. Detect that exact shape and suggest
		// NEXUS_ISLANDS_SRC=<srcName>/src so they don't have to
		// restructure.
		if nested := nestedEntryHint(srcDir, srcName); nested != "" {
			fmt.Fprintf(stdout, "%s●%s frontend watcher: %s has no top-level entry, but %s does — waiting\n",
				ansiCyan, ansiReset, srcName, nested)
			fmt.Fprintf(stdout, "%s   point the bundler at the nested folder with:%s\n",
				ansiCyan, ansiReset)
			fmt.Fprintf(stdout, "      export NEXUS_ISLANDS_SRC=%s\n", nested)
			fmt.Fprintf(stdout, "      nexus dev\n")
			fmt.Fprintf(stdout, "%s   or move your bootstrap up one level into %s/.%s\n",
				ansiCyan, srcName, ansiReset)
		} else {
			fmt.Fprintf(stdout, "%s●%s frontend watcher: %s has files but no top-level .ts/.tsx/.jsx/.js entry — waiting (Ctrl-C to stop)\n",
				ansiCyan, ansiReset, srcName)
			fmt.Fprintf(stdout, "%s   entries are top-level only; nested files are picked up via imports from a bootstrap.%s\n",
				ansiCyan, ansiReset)
		}
	}
	resolved, entries, err := waitForDirEntries(ctx, srcDir)
	return resolved, entries, err
}

// nestedEntryHint peeks one level down for a subdirectory that
// DOES contain valid entries — the Vite-style layout where the
// operator put main.ts under src/ instead of at the root.
// Returns the relative path (e.g. "islands.src/src") when found,
// or "" when no such subdir exists (truly no entries anywhere
// nearby). Checks at most one level deep; deeper nesting is too
// ambiguous to guess at.
func nestedEntryHint(srcDir, srcName string) string {
	dirents, err := os.ReadDir(srcDir)
	if err != nil {
		return ""
	}
	for _, e := range dirents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" || name == "dist" || name == "public" {
			continue
		}
		sub := filepath.Join(srcDir, name)
		entries, err := collectFrontendEntries(sub)
		if err != nil || len(entries) == 0 {
			continue
		}
		return filepath.Join(srcName, name)
	}
	return ""
}

// describeEmptyState classifies why collectFrontendEntries
// returned an empty list, so the wait-loop's log message can
// point the operator at the right fix.
type emptyState int

const (
	emptyStateNoFiles    emptyState = iota // dir contains no non-hidden files at all
	emptyStateOnlyVue                      // dir has .vue (and maybe other) files but no .ts/.tsx/.jsx/.js
	emptyStateNoTopLevel                   // dir has non-vue files / subdirs but still no matching entry
)

func describeEmptyState(srcDir string) emptyState {
	dirents, err := os.ReadDir(srcDir)
	if err != nil {
		return emptyStateNoFiles
	}
	sawAny := false
	sawVue := false
	sawOther := false
	for _, e := range dirents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		sawAny = true
		if e.IsDir() {
			sawOther = true
			continue
		}
		ext := filepath.Ext(name)
		switch ext {
		case ".vue":
			sawVue = true
		case ".jsx", ".tsx", ".ts", ".js":
			// Should not happen: caller already filtered these
			// out via collectFrontendEntries returning > 0.
			// Treat as "no top-level" for safety.
			sawOther = true
		default:
			sawOther = true
		}
	}
	if !sawAny {
		return emptyStateNoFiles
	}
	if sawVue && !sawOther {
		return emptyStateOnlyVue
	}
	return emptyStateNoTopLevel
}

// peekForEntries is findFrontendEntries with a missing-directory
// check that returns (srcDir, nil, nil) instead of a "no such
// file" error. Auto-descends into conventional subdirs (src/ /
// app/ / client/) — that's the headline UX win over the older
// strict top-level-only behavior. Keeps the polling loops above
// one-liner clean.
func peekForEntries(srcDir string) (string, []string, error) {
	if _, err := os.Stat(srcDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return srcDir, nil, nil
		}
		return srcDir, nil, err
	}
	return findFrontendEntries(srcDir)
}

// waitForDirCreate blocks until srcDir exists. Uses an fsnotify
// watch on the parent directory; when an event names srcDir's
// basename, we re-stat to confirm (mkdir + create are separate
// events on some filesystems). 250ms backoff fallback covers
// remote / network filesystems where fsnotify is unreliable.
func waitForDirCreate(ctx context.Context, srcDir string) error {
	parent := filepath.Dir(srcDir)
	want := filepath.Base(srcDir)
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("frontend watcher: fsnotify: %w", err)
	}
	defer w.Close()
	if err := w.Add(parent); err != nil {
		return fmt.Errorf("frontend watcher: watch %s: %w", parent, err)
	}
	// Pre-check: the dir may have been created in the window
	// between the caller's check and our watcher install.
	if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
		return nil
	}
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-w.Events:
			if filepath.Base(ev.Name) != want {
				continue
			}
			if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
				return nil
			}
		case <-poll.C:
			// Defensive re-stat in case fsnotify missed the event
			// (NFS, Docker volume mounts, etc.).
			if info, err := os.Stat(srcDir); err == nil && info.IsDir() {
				return nil
			}
		case err := <-w.Errors:
			return fmt.Errorf("frontend watcher: %w", err)
		}
	}
}

// waitForDirEntries blocks until findFrontendEntries on srcDir
// returns a non-empty list — top-level OR via auto-descent into
// src/ / app/ / client/. fsnotify on srcDir catches file
// creation; the same 500ms poll guards against fsnotify gaps and
// catches files dropped into a not-yet-existing subdir (the
// fsnotify watcher only sees srcDir's direct children).
//
// Returns (actualDir, entries, nil) on success — actualDir is
// where entries were FOUND, which may differ from srcDir under
// auto-descent. (srcDir, nil, nil) on ctx cancellation.
func waitForDirEntries(ctx context.Context, srcDir string) (string, []string, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return srcDir, nil, fmt.Errorf("frontend watcher: fsnotify: %w", err)
	}
	defer w.Close()
	if err := w.Add(srcDir); err != nil {
		return srcDir, nil, fmt.Errorf("frontend watcher: watch %s: %w", srcDir, err)
	}
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	check := func() (string, []string, error) {
		actualDir, entries, err := findFrontendEntries(srcDir)
		if err != nil {
			return srcDir, nil, err
		}
		if len(entries) > 0 {
			return actualDir, entries, nil
		}
		return srcDir, nil, nil
	}
	if actualDir, entries, err := check(); err != nil || entries != nil {
		return actualDir, entries, err
	}
	for {
		select {
		case <-ctx.Done():
			return srcDir, nil, nil
		case <-w.Events:
			actualDir, entries, err := check()
			if err != nil {
				return srcDir, nil, err
			}
			if entries != nil {
				return actualDir, entries, nil
			}
		case <-poll.C:
			actualDir, entries, err := check()
			if err != nil {
				return srcDir, nil, err
			}
			if entries != nil {
				return actualDir, entries, nil
			}
		case err := <-w.Errors:
			return srcDir, nil, fmt.Errorf("frontend watcher: %w", err)
		}
	}
}

// findProjectTSConfig locates the tsconfig.json (or jsconfig.json)
// the bundler should hand to esbuild. Search order:
//
//  1. <root>/islands.src/tsconfig.json — the convention for
//     projects whose Vue/React source lives in islands.src/.
//     This is also where the operator typically defines
//     `paths` like `"@/*": ["./src/*"]` because the alias
//     points INSIDE islands.src/.
//
//  2. <root>/islands.src/jsconfig.json — same shape but for
//     JS-only projects (no TypeScript).
//
//  3. <root>/tsconfig.json — fallback when there's no
//     islands.src-local config. Used when the project has a
//     single root tsconfig that covers everything.
//
//  4. <root>/jsconfig.json — final fallback.
//
// Returns "" when no config exists; the bundler then runs with
// esbuild's default behavior (auto-discovery still walks up
// from input files, so this just means we couldn't lock onto
// one explicit path).
//
// Always returns an absolute path so esbuild's baseUrl
// resolution works correctly regardless of the bundler's cwd.
func findProjectTSConfig(root string) string {
	candidates := []string{
		filepath.Join(root, islandsSrcName(), "tsconfig.json"),
		filepath.Join(root, islandsSrcName(), "jsconfig.json"),
		filepath.Join(root, "tsconfig.json"),
		filepath.Join(root, "jsconfig.json"),
	}
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}
