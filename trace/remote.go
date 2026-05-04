package trace

import "context"

// ParseTraceparent decodes a W3C traceparent header value and returns
// the trace + parent span IDs. Public wrapper around the package-
// private parser so cross-process integrations (pubsub consumers,
// queue workers, gRPC interceptors) can extract the upstream trace
// without each implementing W3C parsing.
//
//	traceID, parentSpanID, ok := trace.ParseTraceparent(headers["traceparent"])
//
// Returns ok=false on any of: empty input, wrong number of segments,
// non-"00" version byte, wrong-length / non-hex IDs, all-zero IDs.
// Callers should fall back to minting a fresh trace when ok=false.
func ParseTraceparent(h string) (traceID, parentSpanID string, ok bool) {
	return parseTraceparent(h)
}

// WithBus stashes bus in ctx so SpanFromCtx / BusFromCtx readers
// downstream can find it. Used by code that builds a fresh ctx
// outside an HTTP request lifecycle (workers, cron jobs, message
// consumers) and wants its emitted spans to land on the dashboard's
// trace stream.
//
// nil bus is treated as "leave ctx alone" — the caller doesn't have
// to guard with `if bus != nil { ctx = WithBus(ctx, bus) }` at every
// site.
func WithBus(ctx context.Context, bus *Bus) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if bus == nil {
		return ctx
	}
	return context.WithValue(ctx, busCtxKey{}, bus)
}

// WithRemoteParent installs a synthetic parent span in ctx so a
// subsequent StartSpan inherits the given trace + parent IDs. The
// returned ctx represents "we are continuing an upstream trace that
// began in another process / another goroutine".
//
// The synthetic span is a placeholder — it does NOT emit a
// span.start event, and it should never have End() called on it
// (the ended flag is set so an accidental call is a no-op). Its
// only purpose is to be visible to SpanFromCtx so child StartSpan
// calls can read TraceID + SpanID off of it as their parent linkage.
//
// Typical use from a pubsub consumer:
//
//	if tid, pid, ok := trace.ParseTraceparent(msg.Attrs["traceparent"]); ok {
//	    ctx = trace.WithRemoteParent(ctx, tid, pid, "subscriber", "topic:sub")
//	}
//	ctx, span := trace.StartSpan(ctx, "pubsub.consume:...", ...)
//
// Empty traceID is treated as "no remote info" — ctx is returned
// unchanged so callers don't have to special-case missing headers.
func WithRemoteParent(ctx context.Context, traceID, parentSpanID, service, endpoint string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(traceID) != 32 {
		return ctx
	}
	bus, _ := BusFromCtx(ctx)
	parent := &Span{
		TraceID:  traceID,
		SpanID:   parentSpanID,
		Service:  service,
		Endpoint: endpoint,
		Remote:   true,
		bus:      bus,
		ended:    true, // sentinel: never emit; never accept End()
	}
	return context.WithValue(ctx, spanCtxKey{}, parent)
}