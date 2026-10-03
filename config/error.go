package config

import (
	"errors"
	"fmt"

	"github.com/paulmanoni/nexus/v2/internal/bootui"
	"github.com/paulmanoni/nexus/v2/manifest"
	"github.com/pelletier/go-toml/v2"
)

// Error describes a boot-blocking problem in the operator's
// nexus.toml — a missing env var, a TOML syntax error, a malformed
// block. It is structured (source, line, cause, suggested fix) so the
// fatal-render path can print a pointed diagnostic instead of a Go
// panic trace: a config mistake is an operator error, not a bug, and
// should never read like a crash.
type Error struct {
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
// error themselves (Load returns it like any other error).
func (e *Error) Error() string {
	loc := e.Source
	if e.Line > 0 {
		loc = fmt.Sprintf("%s: line %d", e.Source, e.Line)
	}
	return fmt.Sprintf("nexus: %s in %s: %v", e.Stage, loc, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// newConfigError classifies err from one load stage into an Error,
// pulling out whatever structure the underlying error carries: the
// failing line and missing variable from manifest's ${VAR} expansion,
// or the position + annotated snippet from go-toml's DecodeError.
func newConfigError(stage, source string, err error) *Error {
	ce := &Error{Source: source, Stage: stage, Err: err}

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

// BootLocation describes the error for the boot diagnostic block.
func (e *Error) BootLocation() bootui.Location {
	return bootui.Location{Stage: e.Stage, Source: e.Source, Line: e.Line, Cause: e.Err, Hint: e.Hint, Snippet: e.Snippet}
}

// NewError classifies err from one load stage of source into an *Error,
// pulling out the line, missing variable or annotated snippet it carries.
func NewError(stage, source string, err error) *Error { return newConfigError(stage, source, err) }
