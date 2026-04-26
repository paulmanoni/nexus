package nexus

import (
	"errors"
	"fmt"
	"strings"

	"github.com/paulmanoni/nexus/trace"
)

// UserError is the framework's developer-facing error envelope. Fields
// produce a multi-line format with an optional `hint:` recipe and an
// optional `cause:` wrap so a developer hitting a framework error sees
// what went wrong, what to do about it, and the underlying cause in
// one block — instead of having to chase the message through several
// fmt.Errorf wraps.
//
//	nexus error [topology]: Deployment "users-svc" not in Topology.Peers
//	  declared peers: [checkout-svc orders-svc]
//	  hint: add Topology.Peers["users-svc"] in main.go's nexus.Config — URL may be empty for the active unit
//
// User code typically doesn't construct these — the framework emits
// them at known failure boundaries. They behave like normal errors
// (Error / Unwrap), so existing error-handling paths work unchanged.
type UserError struct {
	Op    string   // short verb-noun: "topology", "remote call", "path expand"
	Msg   string   // primary one-line description
	Notes []string // optional context lines (peer list, body snippet, etc.)
	Hint  string   // optional fix recipe; single line
	Cause error    // optional wrap — accessible via errors.Unwrap
}

func (e *UserError) Error() string {
	var b strings.Builder
	b.WriteString("nexus error")
	if e.Op != "" {
		b.WriteString(" [")
		b.WriteString(e.Op)
		b.WriteString("]")
	}
	b.WriteString(": ")
	b.WriteString(e.Msg)
	for _, n := range e.Notes {
		if n == "" {
			continue
		}
		b.WriteString("\n  ")
		b.WriteString(n)
	}
	if e.Cause != nil {
		b.WriteString("\n  cause: ")
		b.WriteString(e.Cause.Error())
	}
	if e.Hint != "" {
		b.WriteString("\n  hint: ")
		b.WriteString(e.Hint)
	}
	return b.String()
}

func (e *UserError) Unwrap() error { return e.Cause }

// userErrorf is the internal shorthand for building a UserError with a
// formatted message. Hint and Notes are set by the caller after.
func userErrorf(op, format string, args ...any) *UserError {
	return &UserError{Op: op, Msg: fmt.Sprintf(format, args...)}
}

// truncate returns s clipped to maxLen with a trailing ellipsis when
// it had to clip. Used to embed response-body snippets in errors
// without dumping a 5MB payload into a log line.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}

// attachUserErrorAttrs sets the structured fields of a *UserError as
// span attributes so the dashboard can render `op`, `hint`, `notes`,
// and the underlying cause as separate UI elements instead of a
// single flattened error string. Called from cross-service call
// wrappers (LocalInvoker / RemoteCaller) before span.End fires.
//
// Safe to call with a nil span or non-UserError — both no-ops.
func attachUserErrorAttrs(span *trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	var ue *UserError
	if !errors.As(err, &ue) {
		return
	}
	if ue.Op != "" {
		span.Set("error.op", ue.Op)
	}
	if ue.Hint != "" {
		span.Set("error.hint", ue.Hint)
	}
	if len(ue.Notes) > 0 {
		span.Set("error.notes", strings.Join(ue.Notes, " | "))
	}
	if ue.Cause != nil {
		span.Set("error.cause", ue.Cause.Error())
	}
}