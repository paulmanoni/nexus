// Package trace captures request-lifecycle events (start, end, downstream calls,
// logs) into an in-memory ring buffer and fans them out to subscribers such as
// the dashboard. It is the data plane under /__nexus/events.
package trace

import (
	"errors"
	"strings"
	"time"
)

type Kind string

const (
	KindRequestStart Kind = "request.start"
	KindRequestEnd   Kind = "request.end"
	// KindRequestOp is emitted by the metrics middleware AFTER the
	// handler returns, carrying the specific op name in Endpoint so
	// UI consumers can drive per-endpoint visualisations (packet
	// animations, per-op trace rows). request.start from the trace
	// middleware uses the HTTP path — too coarse to identify a
	// GraphQL operation; request.op fills that gap.
	KindRequestOp Kind = "request.op"
	// KindDownstream is an untimed "X happened during this request"
	// marker (e.g. resource.using lookup). For timed sub-operations use
	// KindSpanStart / KindSpanEnd so they render as bars in the
	// waterfall.
	KindDownstream Kind = "downstream"
	KindLog        Kind = "log"
	// KindSpanStart / KindSpanEnd bracket a child span. Emitted by
	// StartSpan / (*Span).End and by trace.Record. The ParentID field
	// links children to their parent (the root request or an enclosing
	// span); the dashboard reconstructs the tree from it.
	KindSpanStart Kind = "span.start"
	KindSpanEnd   Kind = "span.end"
)

type Event struct {
	ID         int64  `json:"id"`
	TraceID    string `json:"traceId"`
	SpanID     string `json:"spanId,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
	Name       string `json:"name,omitempty"`
	Kind       Kind   `json:"kind"`
	Service    string `json:"service,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	Transport  string `json:"transport,omitempty"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	Status     int    `json:"status,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	// Stack carries the captured Go runtime stack trace for events
	// that originated from a panic. The framework's recovery
	// middleware wraps the panic value in a *StackError; metrics +
	// trace publishers extract the stack via StackOf and attach it
	// here so the dashboard's error views can show "where did this
	// come from?" without operators tailing logs.
	Stack     string         `json:"stack,omitempty"`
	Remote    bool           `json:"remote,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// StackError is an error that carries a captured Go stack trace.
// Used by the framework's panic-recovery middleware to thread both
// the panic value AND the stack through gin's c.Error/c.Errors path,
// so downstream middleware (metrics, trace) can surface the stack
// on the dashboard via StackOf.
//
// Direct use:
//
//	&trace.StackError{Err: fmt.Errorf("..."), Stack: string(debug.Stack())}
//
// Errors that don't originate from a panic don't get a stack
// automatically; user code can opt in by wrapping at the source.
type StackError struct {
	Err   error
	Stack string
}

func (e *StackError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

// Unwrap lets errors.Is / errors.As / errors.Unwrap walk past the
// stack-carrier to the underlying error value.
func (e *StackError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StackOf walks an error chain via errors.As and returns the first
// captured stack trace, or "" when none is present. Lets observers
// (metrics middleware, trace publishers) extract the stack without
// needing to know whether the caller wrapped before returning.
func StackOf(err error) string {
	if err == nil {
		return ""
	}
	var s *StackError
	if errors.As(err, &s) && s != nil {
		return s.Stack
	}
	return ""
}

// CleanStack trims the runtime + recovery noise from a debug.Stack()
// output so the user-code frame ends up at the top.
//
// Raw debug.Stack() looks like:
//
//	goroutine 25 [running]:
//	runtime/debug.Stack()
//	    .../runtime/debug/stack.go:26 +0x64
//	github.com/.../metrics.ginRecorder.func2.1()
//	    .../metrics/middleware.go:62 +0x148
//	panic({0x...})
//	    .../runtime/panic.go:860 +0x12c
//	github.com/.../users.NewBoom(...)            ← actual panic site
//	    .../examples/microsplit/users/handlers.go:66 +0x28
//	...
//
// The first three frames (Stack itself, the deferred recover, panic)
// are framework noise — they shift on every Go release and tell the
// user nothing about their bug. We strip everything up to and
// including the panic frame so the cleaned stack starts at the user
// code. Returns the input unchanged when no `panic(` line is found
// (defensive — keeps the dashboard usable on edge-case inputs).
func CleanStack(raw string) string {
	if raw == "" {
		return ""
	}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "panic(") {
			// Drop the panic line + its file-path line that follows;
			// keep the remaining frames + the goroutine header.
			start := i + 2
			if start >= len(lines) {
				return raw
			}
			head := "goroutine [running]:"
			// Preserve original goroutine header if present so the
			// reader still has the goroutine id.
			if len(lines) > 0 && strings.HasPrefix(lines[0], "goroutine") {
				head = lines[0]
			}
			return head + "\n" + strings.Join(lines[start:], "\n")
		}
	}
	return raw
}
