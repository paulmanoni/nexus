package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/evanw/esbuild/pkg/api"
)

const tag = "[web] "

// fakeBuild builds a vite-style rebuild block with the given asset
// table lines. Mimics the shape vite emits so the filter exercises
// realistic input.
func fakeBuild(durationMs int, assets ...string) []string {
	lines := []string{
		"build started...",
		"transforming...",
		"✓ 1084 modules transformed.",
		"rendering chunks...",
		"computing gzip size...",
	}
	lines = append(lines, assets...)
	lines = append(lines, "")
	lines = append(lines, "built in "+itoa(durationMs)+"ms.")
	return lines
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestBuildBlockFilter_FirstCycleEmittedVerbatim verifies the first
// rebuild block streams through unfiltered so the user sees the
// initial bundle shape.
func TestBuildBlockFilter_FirstCycleEmittedVerbatim(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	for _, l := range fakeBuild(2000,
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/main-AAA.js  389.02 kB │ gzip: 109.45 kB",
	) {
		f.line(l)
	}
	got := out.String()
	for _, want := range []string{
		"build started...",
		"dist/index.html  1.14 kB",
		"dist/assets/main-AAA.js  389.02 kB",
		"built in 2000ms",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("first cycle missing %q\n--- output ---\n%s", want, got)
		}
	}
}

// TestBuildBlockFilter_IdenticalRebuildIsSilent verifies the loop
// signature: when the second cycle's asset table matches the first,
// the filter emits NOTHING. A no-change rebuild loop can fire
// dozens of cycles per minute — any per-cycle output spams the
// terminal. Silence is the only acceptable default.
func TestBuildBlockFilter_IdenticalRebuildIsSilent(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	assets := []string{
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/main-AAAAAAAA.js  389.02 kB │ gzip: 109.45 kB",
	}
	for _, l := range fakeBuild(2000, assets...) {
		f.line(l)
	}
	out.Reset()
	// Three identical rebuild cycles + the blank "spacer" lines
	// vite emits between them — all of it should produce nothing.
	for i := 0; i < 3; i++ {
		f.line("") // vite's between-cycle blank
		for _, l := range fakeBuild(1850+i, assets...) {
			f.line(l)
		}
	}
	if got := out.String(); got != "" {
		t.Errorf("identical rebuilds should emit nothing; got:\n%s", got)
	}
}

// TestBuildBlockFilter_HashShuffleSuppressed pins the load-bearing
// case: vite often re-hashes many chunks on any source edit
// because module IDs reshuffle. If the SIZE didn't change, those
// hash-only diffs are noise and must collapse like a no-changes
// rebuild. Only files whose size genuinely shifted should print.
func TestBuildBlockFilter_HashShuffleSuppressed(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	for _, l := range fakeBuild(2000,
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/index-Df8jaTme.js  389.02 kB │ gzip: 109.45 kB",
		"dist/assets/Login-AAAAAAAA.js  2.23 kB │ gzip:  1.15 kB",
		"dist/assets/Roles-BBBBBBBB.js  4.55 kB │ gzip:  1.95 kB",
	) {
		f.line(l)
	}
	out.Reset()
	// Login.vue edited → only Login's size changes. vite re-hashes
	// index AND Roles even though their bytes are identical.
	for _, l := range fakeBuild(1900,
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/index-XXXXXXXX.js  389.02 kB │ gzip: 109.45 kB",
		"dist/assets/Login-YYYYYYYY.js  2.30 kB │ gzip:  1.18 kB",
		"dist/assets/Roles-ZZZZZZZZ.js  4.55 kB │ gzip:  1.95 kB",
	) {
		f.line(l)
	}
	got := out.String()
	if !strings.Contains(got, "Login.js") {
		t.Errorf("expected Login.js to appear (size changed), got\n%s", got)
	}
	if !strings.Contains(got, "2.23 → 2.30 kB") {
		t.Errorf("expected size-delta '2.23 → 2.30 kB' for Login.js, got\n%s", got)
	}
	for _, drop := range []string{"index-XXXXXXXX", "Roles-ZZZZZZZZ", "index-Df8jaTme"} {
		if strings.Contains(got, drop) {
			t.Errorf("hash-shuffled asset %q leaked into output\n%s", drop, got)
		}
	}
}

// TestBuildBlockFilter_AddRemoveDistinct verifies the genuine
// add/remove case: a brand-new logical asset appears with "+"
// and one that disappeared shows with "-".
func TestBuildBlockFilter_AddRemoveDistinct(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	for _, l := range fakeBuild(2000,
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/Old-AAAAAAAA.js  2.23 kB │ gzip:  1.15 kB",
	) {
		f.line(l)
	}
	out.Reset()
	for _, l := range fakeBuild(1900,
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/New-BBBBBBBB.js  3.40 kB │ gzip:  1.50 kB",
	) {
		f.line(l)
	}
	got := out.String()
	if !strings.Contains(got, "+ ") || !strings.Contains(got, "New.js") {
		t.Errorf("expected '+ New.js' line, got\n%s", got)
	}
	if !strings.Contains(got, "- ") || !strings.Contains(got, "Old.js") {
		t.Errorf("expected '- Old.js' line, got\n%s", got)
	}
}

// TestViteURLRE verifies the regex pulls vite's "Local: http://..."
// out of the various shapes its dev server prints across versions.
// Capture group 1 must hold the full URL (with or without trailing
// slash) because waitAndOpen normalizes that.
func TestViteURLRE(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  ➜  Local:   http://localhost:5173/", "http://localhost:5173/"},
		{"Local: http://127.0.0.1:5174/", "http://127.0.0.1:5174/"},
		{"  Local:    https://localhost:8443/", "https://localhost:8443/"},
		// Negative case — bundle-mode noise shouldn't false-match.
		{"build started...", ""},
		{"dist/index.html  1.14 kB", ""},
	}
	for _, tc := range cases {
		m := viteURLRE.FindStringSubmatch(tc.in)
		var got string
		if len(m) > 1 {
			got = m[1]
		}
		if got != tc.want {
			t.Errorf("input %q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBuildBlockFilter_NonBuildLinesPassThrough verifies that
// vite output OUTSIDE a build cycle (HMR notices, warnings,
// arbitrary plugin chatter) streams through verbatim.
func TestBuildBlockFilter_NonBuildLinesPassThrough(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	f.line("watching for file changes...")
	f.line("[unplugin-vue-components] component conflict")
	got := out.String()
	for _, want := range []string{
		"watching for file changes",
		"unplugin-vue-components",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("non-build line %q dropped", want)
		}
	}
}
func TestIsVendorDiag(t *testing.T) {
	cases := []struct {
		name string
		msg  api.Message
		want bool
	}{
		{"vendor dep", api.Message{Location: &api.Location{File: "nexus-deps:https://esm.sh/pdfmake@0.3.5/build/pdfmake.mjs"}}, true},
		{"user source", api.Message{Location: &api.Location{File: "islands.src/src/App.vue"}}, false},
		{"no location", api.Message{Text: "some global warning"}, false},
		{"lookalike prefix", api.Message{Location: &api.Location{File: "nexus-deps-fake/x.js"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isVendorDiag(tc.msg); got != tc.want {
				t.Errorf("isVendorDiag(%q) = %v, want %v", tc.msg.Location, got, tc.want)
			}
		})
	}
}

func TestReporter_HidesVendorWarnings(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := newBundlerReporter(&stdout, &stderr, tag, false) // verbose=false
	r.report(api.BuildResult{
		Warnings: []api.Message{
			{Text: "Duplicate key", Location: &api.Location{File: "nexus-deps:https://esm.sh/pdfmake.mjs", Line: 1, Column: 1}},
			{Text: "user mistake", Location: &api.Location{File: "islands.src/src/App.vue", Line: 3, Column: 5}},
		},
	})
	out := stderr.String()
	if strings.Contains(out, "Duplicate key") {
		t.Errorf("vendor warning should be hidden; stderr:\n%s", out)
	}
	if !strings.Contains(out, "user mistake") {
		t.Errorf("user warning should be shown; stderr:\n%s", out)
	}
	if !strings.Contains(out, "1 warning from cached dependencies hidden") {
		t.Errorf("expected suppressed tally; stderr:\n%s", out)
	}
}

func TestReporter_VerboseShowsVendorWarnings(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := newBundlerReporter(&stdout, &stderr, tag, true) // verbose=true
	r.report(api.BuildResult{
		Warnings: []api.Message{
			{Text: "Duplicate key", Location: &api.Location{File: "nexus-deps:https://esm.sh/pdfmake.mjs", Line: 1, Column: 1}},
		},
	})
	if !strings.Contains(stderr.String(), "Duplicate key") {
		t.Errorf("verbose should show vendor warnings; stderr:\n%s", stderr.String())
	}
}

func TestBundlerReporter_SummarizesByDefault(t *testing.T) {
	mkResult := func(names ...string) api.BuildResult {
		var out []api.OutputFile
		for _, n := range names {
			out = append(out, api.OutputFile{Path: "/o/" + n, Contents: []byte("x")})
		}
		return api.BuildResult{OutputFiles: out}
	}

	var stdout, stderr bytes.Buffer
	r := newBundlerReporter(&stdout, &stderr, "[web] ", false)

	// First build: one summary line, NOT a per-file dump.
	r.report(mkResult("main.js", "chunks/A-1.js", "chunks/B-1.js"))
	first := stdout.String()
	if !strings.Contains(first, "bundled 3 files") {
		t.Errorf("first build should summarize; got:\n%s", first)
	}
	if strings.Contains(first, "+ chunks/A-1.js") || strings.Contains(first, "A-1.js  ") {
		t.Errorf("first build leaked per-file lines:\n%s", first)
	}

	// Rebuild that re-hashes a chunk (A-1 → A-2): summary, not a list.
	stdout.Reset()
	r.report(mkResult("main.js", "chunks/A-2.js", "chunks/B-1.js"))
	rb := stdout.String()
	if !strings.Contains(rb, "rebuilt —") {
		t.Errorf("rebuild should summarize; got:\n%s", rb)
	}
	if strings.Contains(rb, "chunks/A-2.js") {
		t.Errorf("rebuild leaked per-file lines:\n%s", rb)
	}
}

func TestBundlerReporter_VerboseListsFiles(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := newBundlerReporter(&stdout, &stderr, "[web] ", true)
	res := api.BuildResult{OutputFiles: []api.OutputFile{{Path: "/o/main.js", Contents: []byte("x")}}}
	r.report(res)
	if !strings.Contains(stdout.String(), "main.js") {
		t.Errorf("verbose first build should list files; got:\n%s", stdout.String())
	}
}
