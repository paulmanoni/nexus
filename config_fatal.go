package nexus

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/paulmanoni/nexus/manifest"
	"github.com/pelletier/go-toml/v2"
)

// ConfigError describes a boot-blocking problem in the operator's
// nexus.toml — a missing env var, a TOML syntax error, a malformed
// block. It is structured (source, line, cause, suggested fix) so the
// fatal-render path can print a pointed diagnostic instead of a Go
// panic trace: a config mistake is an operator error, not a bug, and
// should never read like a crash.
type ConfigError struct {
	// Source is the config's origin: a file path, or "embedded
	// nexus.toml" for the build-time embed.
	Source string
	// Stage names the load step that failed ("expand env vars",
	// "parse", "decode [extensions.*]").
	Stage string
	// Line is the 1-based line of the failing token; 0 when unknown.
	Line int
	// Err is the underlying cause.
	Err error
	// Hint, when non-empty, is a one-line suggested fix.
	Hint string
	// Snippet, when non-empty, is a multi-line annotated excerpt of
	// the offending TOML (go-toml provides one for syntax errors).
	Snippet string
}

// Error keeps the flat single-line form for callers that handle the
// error themselves (LoadConfig returns it like any other error).
func (e *ConfigError) Error() string {
	loc := e.Source
	if e.Line > 0 {
		loc = fmt.Sprintf("%s: line %d", e.Source, e.Line)
	}
	return fmt.Sprintf("nexus: %s in %s: %v", e.Stage, loc, e.Err)
}

func (e *ConfigError) Unwrap() error { return e.Err }

// newConfigError classifies err from one load stage into a ConfigError,
// pulling out whatever structure the underlying error carries: the
// failing line and missing variable from manifest's ${VAR} expansion,
// or the position + annotated snippet from go-toml's DecodeError.
func newConfigError(stage, source string, err error) *ConfigError {
	ce := &ConfigError{Source: source, Stage: stage, Err: err}

	var xe *manifest.ExpandError
	if errors.As(err, &xe) {
		ce.Line = xe.Line
		ce.Err = xe.Err // the line renders once, in the location — not again in the cause
		var me *manifest.MissingEnvError
		if errors.As(err, &me) {
			ce.Err = fmt.Errorf("env var %s is not set", me.Var)
			ce.Hint = fmt.Sprintf("export %s=…  — or write ${%s:default} in %s for a fallback",
				me.Var, me.Var, source)
		}
	}

	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, _ := de.Position()
		ce.Line = row
		ce.Snippet = de.String()
	}
	return ce
}

// ── fatal rendering ─────────────────────────────────────────────────

// bootFatal renders err as a structured diagnostic on stderr and
// exits with status 2. Must* and Boot route config errors here: they
// are operator mistakes, and a panic's goroutine dump buries the one
// line the operator needs under forty they don't.
func bootFatal(err error) {
	renderBootError(os.Stderr, err, bootColorsEnabled())
	os.Exit(2)
}

// bootColorsEnabled decides whether the fatal block uses ANSI color:
// yes when stderr is a terminal, or when running under `nexus dev`
// (the child's pipe isn't a tty, but the dev loop's terminal is and
// passes ANSI through). NO_COLOR always wins.
func bootColorsEnabled() bool {
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

// renderBootError writes the structured block. Shape:
//
//	✗ nexus: cannot load nexus.toml
//
//	  file   /path/to/nexus.toml:139
//	  error  env var OATS_DB_PASSWORD is not set
//	  fix    export OATS_DB_PASSWORD=…  — or write ${OATS_DB_PASSWORD:default} in nexus.toml for a fallback
func renderBootError(w io.Writer, err error, color bool) {
	red, yellow, cyan, bold, dim, reset := "", "", "", "", "", ""
	if color {
		red, yellow, cyan = "\x1b[31m", "\x1b[33m", "\x1b[36m"
		bold, dim, reset = "\x1b[1m", "\x1b[2m", "\x1b[0m"
	}
	row := func(label, val string) {
		fmt.Fprintf(w, "  %s%-6s%s %s\n", dim, label, reset, val)
	}

	var ce *ConfigError
	if !errors.As(err, &ce) {
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

	fmt.Fprintf(w, "\n%s%s✗ nexus: cannot load config%s %s(%s)%s\n\n",
		red, bold, reset, dim, ce.Stage, reset)
	loc := ce.Source
	if ce.Line > 0 {
		loc = fmt.Sprintf("%s:%d", ce.Source, ce.Line)
	}
	row("file", cyan+loc+reset)
	row("error", red+ce.Err.Error()+reset)
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
