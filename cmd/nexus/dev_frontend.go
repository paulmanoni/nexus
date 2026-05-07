package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// startFrontendWatcher spawns the user's frontend build watcher
// (typically `vite build --watch` wrapped in an npm script) inside
// dir, prefixing every output line with [web] so the combined log
// stream stays readable.
//
// verbose=false (default) routes stdout through a build-block
// dedup filter: the first occurrence of any rebuild block streams
// in full, but consecutive blocks producing the BYTE-IDENTICAL
// output collapse into a single "● identical rebuild suppressed"
// summary. Real source edits change the bundle, so they pass
// through; the auto-import-plugin self-rebuild loop produces
// identical bundles every cycle and gets compressed.
//
// verbose=true bypasses the filter, streaming everything verbatim
// for users who want to inspect the full output.
//
// stderr is always streamed verbatim regardless of verbose so real
// build errors surface immediately.
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
func startFrontendWatcher(ctx context.Context, dir, cmdline string, verbose bool, stdout, stderr io.Writer) error {
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
	stderrPump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			fmt.Fprintf(dst, "%s%s\n", tag, scanner.Text())
		}
	}
	stdoutPump := func(src io.Reader, dst io.Writer) {
		defer wg.Done()
		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		if verbose {
			for scanner.Scan() {
				fmt.Fprintf(dst, "%s%s\n", tag, scanner.Text())
			}
			return
		}
		f := newBuildBlockFilter(dst, tag)
		for scanner.Scan() {
			f.line(scanner.Text())
		}
		f.flush()
	}
	wg.Add(2)
	go stdoutPump(stdoutPipe, stdout)
	go stderrPump(stderrPipe, stderr)

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
		fmt.Fprintf(f.dst, "%s%s● rebuild · no changes (%s)%s\n",
			f.tag, ansiDim, duration, ansiReset)
	} else {
		fmt.Fprintf(f.dst, "%s● %d changed · %d unchanged (%s)\n",
			f.tag, len(added)+len(removed)+len(changed), unchanged, duration)
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
