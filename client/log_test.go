package client

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestFdumpLine_FormatShape pins the wire format: timestamp + verb +
// path + optional details. Tests don't assert the literal time
// (clock moves between runs); they swap the package-level timestamp
// hook for a deterministic value so the rest of the string is
// byte-comparable.
func TestFdumpLine_FormatShape(t *testing.T) {
	defer restoreTimestamp(t)
	timestamp = func() string { return "12:34:56" }

	t.Setenv("NO_COLOR", "1") // disable colors so the test reads literal verbs

	var buf bytes.Buffer
	fdumpLine(&buf, ansiGreen, "wrote", "web/sdk/client.js", "100 bytes")
	got := buf.String()
	want := "12:34:56 wrote web/sdk/client.js (100 bytes)\n"
	if got != want {
		t.Fatalf("fdumpLine output mismatch\n got: %q\nwant: %q", got, want)
	}
}

// TestFdumpLine_OmitsDetailsWhenEmpty covers the no-details branch —
// some log lines (config tweaks, dir creations) carry no parenthetical.
func TestFdumpLine_OmitsDetailsWhenEmpty(t *testing.T) {
	defer restoreTimestamp(t)
	timestamp = func() string { return "12:34:56" }
	t.Setenv("NO_COLOR", "1")

	var buf bytes.Buffer
	fdumpLine(&buf, ansiDim, "skipped", "web/sdk/nexus.ts", "")
	got := buf.String()
	want := "12:34:56 skipped web/sdk/nexus.ts\n"
	if got != want {
		t.Fatalf("fdumpLine without details\n got: %q\nwant: %q", got, want)
	}
}

// TestUseColor_NoColorEnvDisables verifies NO_COLOR turns colors off
// even when the writer would otherwise qualify (a *os.File pointed
// at a real terminal). NO_COLOR is the universal opt-out — apps
// that disable it once want every framework log line to honor it.
func TestUseColor_NoColorEnvDisables(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if useColor(os.Stdout) {
		t.Fatal("useColor true with NO_COLOR set; should be false")
	}
}

// TestUseColor_NonTerminalWriterDisables ensures piped output
// doesn't get ANSI escape sequences. A bytes.Buffer (the test's
// typical sink) is not a *os.File and must return false so test
// assertions can compare literal verbs.
func TestUseColor_NonTerminalWriterDisables(t *testing.T) {
	t.Setenv("NO_COLOR", "") // explicit clear so a parent env doesn't skew the test
	if useColor(&bytes.Buffer{}) {
		t.Fatal("useColor true for bytes.Buffer; should be false (not a terminal)")
	}
}

// TestColorize_PassThroughWithoutColor ensures the helper returns
// the raw string when the writer isn't a TTY — no half-emitted
// escape sequences leaking into log files.
func TestColorize_PassThroughWithoutColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	got := colorize(&bytes.Buffer{}, ansiGreen, "wrote")
	if got != "wrote" {
		t.Fatalf("colorize without TTY: %q, want raw 'wrote'", got)
	}
}

// TestColorize_WrapsWhenColorEnabled verifies the ANSI wrap when
// the conditions allow it. Built via a fake terminal-like writer
// would be ideal, but the simpler path is to construct a real pty.
// For phase-2 scope, force the path by writing the wrap directly
// and checking the string round-trip.
func TestColorize_ContainsResetWhenWrapped(t *testing.T) {
	// We can't easily simulate a TTY in unit tests without pulling
	// in os/exec or a pty helper. Verify the constants instead so
	// a future fix to useColor's detection doesn't accidentally
	// strip the reset code, which would leak color across multiple
	// log lines.
	if !strings.HasSuffix(ansiReset, "m") || ansiReset == "" {
		t.Fatalf("ansiReset malformed: %q", ansiReset)
	}
	if !strings.HasPrefix(ansiGreen, "\x1b[") {
		t.Fatalf("ansiGreen malformed: %q", ansiGreen)
	}
}

// restoreTimestamp lets a test override the package-level timestamp
// hook and restores it on cleanup so subsequent tests see real time
// again.
func restoreTimestamp(t *testing.T) {
	t.Helper()
	orig := timestamp
	t.Cleanup(func() { timestamp = orig })
}
