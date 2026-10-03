// Package bootui prints the diagnostic block a failed boot exits with: a
// config error with its file, line and fix, or any other startup error.
package bootui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Location is where a config error sits, as the diagnostic block shows it.
type Location struct {
	Stage, Source string
	Line          int
	Cause         error
	Hint, Snippet string
}

// ── fatal rendering ─────────────────────────────────────────────────

// Fatal renders err as a structured diagnostic on stderr and
// exits with status 2. Must* and Boot route config errors here: they
// are operator mistakes, and a panic's goroutine dump buries the one
// line the operator needs under forty they don't.
func Fatal(err error) {
	Render(os.Stderr, err, Colors())
	os.Exit(2)
}

// Colors decides whether the fatal block uses ANSI color:
// yes when stderr is a terminal, or when running under `nexus dev`
// (the child's pipe isn't a tty, but the dev loop's terminal is and
// passes ANSI through). NO_COLOR always wins.
func Colors() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("NEXUS_DEV") == "1" {
		return true
	}
	if fi, err := os.Stderr.Stat(); err == nil {
		return fi.Mode()&os.ModeCharDevice != 0
	}
	return false
}

// Render writes the structured block. Shape:
//
//	✗ nexus: cannot load nexus.toml
//
//	  file   /path/to/nexus.toml:139
//	  error  env var DB_PASSWORD is not set
//	  fix    export DB_PASSWORD=…  — or write ${DB_PASSWORD:default} in nexus.toml for a fallback
func Render(w io.Writer, err error, color bool) {
	red, yellow, cyan, bold, dim, reset := "", "", "", "", "", ""
	if color {
		red, yellow, cyan = "\x1b[31m", "\x1b[33m", "\x1b[36m"
		bold, dim, reset = "\x1b[1m", "\x1b[2m", "\x1b[0m"
	}
	row := func(label, val string) {
		fmt.Fprintf(w, "  %s%-6s%s %s\n", dim, label, reset, val)
	}

	var located interface{ BootLocation() Location }
	if !errors.As(err, &located) {
		fmt.Fprintf(w, "\n%s%s✗ nexus: failed to start%s\n\n", red, bold, reset)
		for i, line := range strings.Split(strings.TrimRight(err.Error(), "\n"), "\n") {
			if i == 0 {
				row("error", red+line+reset)
				continue
			}
			// Continuation lines (a DI error's "needed by …") keep their
			// own indentation rather than being squeezed into the label
			// column.
			fmt.Fprintf(w, "         %s\n", strings.TrimLeft(line, "\t "))
		}
		var h interface{ Hint() string }
		if errors.As(err, &h) && h.Hint() != "" {
			row("fix", yellow+h.Hint()+reset)
		}
		fmt.Fprintln(w)
		return
	}

	ce := located.BootLocation()
	fmt.Fprintf(w, "\n%s%s✗ nexus: cannot load config%s %s(%s)%s\n\n",
		red, bold, reset, dim, ce.Stage, reset)
	loc := ce.Source
	if ce.Line > 0 {
		loc = fmt.Sprintf("%s:%d", ce.Source, ce.Line)
	}
	row("file", cyan+loc+reset)
	row("error", red+ce.Cause.Error()+reset)
	if ce.Hint != "" {
		row("fix", yellow+ce.Hint+reset)
	}
	if ce.Snippet != "" {
		fmt.Fprintln(w)
		for _, l := range strings.Split(strings.TrimRight(ce.Snippet, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	fmt.Fprintln(w)
}
