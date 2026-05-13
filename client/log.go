package client

import (
	"fmt"
	"io"
	"os"
	"time"
)

// ANSI color escape codes used by the auto-dump log lines. Picked
// for legibility on both light and dark terminal themes: green for
// "thing happened", yellow for "thing skipped because it matched",
// dim for "thing already there", red for errors.
const (
	ansiReset  = "\x1b[0m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
)

// useColor returns true when log writes should include ANSI escapes.
// Honors NO_COLOR (the de-facto opt-out convention — see no-color.org)
// and falls back to "color if stdout is a terminal" otherwise. Tests
// can flip the global by clearing NO_COLOR / writing to a real tty;
// the writer-side override (passing a non-tty io.Writer) silences
// colors transparently because the helper checks the writer too.
func useColor(w io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	// IsTerminal-ish without pulling in golang.org/x/term: check
	// that the fd's mode includes the character-device bit, which
	// is how TTYs surface on both unix and macOS. False negatives
	// (some pipes report ModeCharDevice on rare configs) are
	// acceptable — they just disable colors, never the wrong
	// direction.
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

// colorize wraps s in an ANSI color when color output is enabled
// for w; returns s unchanged otherwise. The conditional keeps the
// callsites readable — fmt.Fprintf(out, "%s thing", colorize(out, ansiGreen, "wrote")).
func colorize(w io.Writer, color, s string) string {
	if !useColor(w) {
		return s
	}
	return color + s + ansiReset
}

// timestamp emits a "HH:MM:SS" string for log lines. Short enough to
// blend with the verb + path, long enough to compare events at a
// glance. RFC3339 (with date + timezone) is overkill for a startup
// log that the user is watching in real time.
//
// Hoisted as a function so tests can override it via the package-
// level var; the production path always returns time.Now()'s clock.
var timestamp = func() string {
	return time.Now().Format("15:04:05")
}

// fdumpLine is the shared format for every auto-dump log line:
//
//	<HH:MM:SS> <colored verb> <path> (<details>)
//
// Used by WriteIfChanged, WriteIfMissing, and any future variant —
// keeping the layout in one helper means the dev-log shape is
// consistent across the dump suite, and tests can verify "the dump
// said wrote X" without coupling to exact escape sequences.
func fdumpLine(w io.Writer, color, verb, path, details string) {
	v := colorize(w, color, verb)
	if details == "" {
		fmt.Fprintf(w, "%s %s %s\n", timestamp(), v, path)
		return
	}
	fmt.Fprintf(w, "%s %s %s (%s)\n", timestamp(), v, path, details)
}
