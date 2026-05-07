package main

import (
	"bytes"
	"strings"
	"testing"
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

// TestBuildBlockFilter_IdenticalRebuildSuppressed verifies the loop
// signature: when the second cycle's asset table matches the first,
// only a one-line "no changes" summary appears.
func TestBuildBlockFilter_IdenticalRebuildSuppressed(t *testing.T) {
	var out bytes.Buffer
	f := newBuildBlockFilter(&out, tag)
	assets := []string{
		"dist/index.html  1.14 kB │ gzip:  0.52 kB",
		"dist/assets/main-AAA.js  389.02 kB │ gzip: 109.45 kB",
	}
	for _, l := range fakeBuild(2000, assets...) {
		f.line(l)
	}
	out.Reset() // drop the first-cycle output, focus on the second
	for _, l := range fakeBuild(1850, assets...) {
		f.line(l)
	}
	got := out.String()
	if !strings.Contains(got, "no changes") {
		t.Errorf("expected 'no changes' summary, got\n%s", got)
	}
	// The full asset table must NOT appear — that's the loop spam.
	if strings.Contains(got, "main-AAA.js") {
		t.Errorf("identical asset line leaked into output\n%s", got)
	}
	if strings.Contains(got, "build started") {
		t.Errorf("'build started...' header leaked into suppressed cycle\n%s", got)
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
	if !strings.Contains(got, "1 changed") {
		t.Errorf("expected '1 changed' header, got\n%s", got)
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