// Package trace captures request-lifecycle events (start, end, downstream calls,
// logs) into an in-memory ring buffer and fans them out to subscribers such as
// the dashboard. It is the data plane under /__nexus/events.
package trace

import "time"

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
	ID         int64          `json:"id"`
	TraceID    string         `json:"traceId"`
	SpanID     string         `json:"spanId,omitempty"`
	ParentID   string         `json:"parentId,omitempty"`
	Name       string         `json:"name,omitempty"`
	Kind       Kind           `json:"kind"`
	Service    string         `json:"service,omitempty"`
	Endpoint   string         `json:"endpoint,omitempty"`
	Transport  string         `json:"transport,omitempty"`
	Method     string         `json:"method,omitempty"`
	Path       string         `json:"path,omitempty"`
	Status     int            `json:"status,omitempty"`
	DurationMs int64          `json:"durationMs,omitempty"`
	Message    string         `json:"message,omitempty"`
	Error      string         `json:"error,omitempty"`
	Remote     bool           `json:"remote,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`
	Meta       map[string]any `json:"meta,omitempty"`
}