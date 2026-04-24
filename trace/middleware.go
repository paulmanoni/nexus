package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	spanKey = "nexus.trace.span"
	busKey  = "nexus.trace.bus"
)

type spanCtxKey struct{}
type busCtxKey struct{}

// SpanFromCtx reads the current span from any context.Context — root or child,
// whichever was pushed last. Use this from code paths that don't have a
// *gin.Context (GraphQL resolvers via p.Context, GORM callbacks, worker code
// called from a request).
func SpanFromCtx(ctx context.Context) (*Span, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(spanCtxKey{}).(*Span)
	return s, ok
}

// BusFromCtx returns the Bus from context, if the trace middleware stashed one.
func BusFromCtx(ctx context.Context) (*Bus, bool) {
	if ctx == nil {
		return nil, false
	}
	b, ok := ctx.Value(busCtxKey{}).(*Bus)
	return b, ok
}

// newTraceID mints a random 16-byte (32 hex char) trace ID, matching the W3C
// traceparent format so we can emit inter-op headers and honor inbound ones.
func newTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// NewTraceID mints the next trace ID in the same format the request middleware
// uses. Exposed for code paths that emit events outside the HTTP request
// lifecycle (e.g. the cron scheduler) so trace IDs stay uniform across the
// dashboard.
func NewTraceID() string { return newTraceID() }

// Middleware emits request.start and request.end events bracketing the handler
// chain, and stashes a *Span and the bus in gin.Context so child spans and
// Record() attach to the same trace.
//
// If the inbound request carries a valid W3C traceparent header, the root
// span reuses its TraceID (and records ParentID from the upstream span) so
// the trace is stitched across services. Otherwise a fresh TraceID is minted.
func Middleware(bus *Bus, service, endpoint, transport string) gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID, parentSpanID, remote := parseTraceparent(c.Request.Header.Get("traceparent"))
		if !remote {
			traceID = newTraceID()
			parentSpanID = ""
		}
		span := &Span{
			TraceID:  traceID,
			SpanID:   newSpanID(),
			ParentID: parentSpanID,
			Name:     endpoint,
			Service:  service,
			Endpoint: endpoint,
			Start:    time.Now(),
			Remote:   remote,
			bus:      bus,
		}
		c.Set(spanKey, span)
		c.Set(busKey, bus)
		// Also propagate onto context.Context so ctx-only code (GraphQL
		// resolvers, GORM hooks, any downstream taking a context.Context)
		// can read via SpanFromCtx / BusFromCtx.
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, spanCtxKey{}, span)
		ctx = context.WithValue(ctx, busCtxKey{}, bus)
		c.Request = c.Request.WithContext(ctx)
		bus.Publish(Event{
			TraceID:   span.TraceID,
			SpanID:    span.SpanID,
			ParentID:  span.ParentID,
			Kind:      KindRequestStart,
			Name:      endpoint,
			Service:   service,
			Endpoint:  endpoint,
			Transport: transport,
			Method:    c.Request.Method,
			Path:      c.Request.URL.Path,
			Remote:    span.Remote,
			Timestamp: span.Start,
		})
		c.Next()
		var errStr string
		if len(c.Errors) > 0 {
			errStr = c.Errors.String()
		}
		bus.Publish(Event{
			TraceID:    span.TraceID,
			SpanID:     span.SpanID,
			ParentID:   span.ParentID,
			Kind:       KindRequestEnd,
			Name:       endpoint,
			Service:    service,
			Endpoint:   endpoint,
			Transport:  transport,
			Method:     c.Request.Method,
			Path:       c.Request.URL.Path,
			Status:     c.Writer.Status(),
			DurationMs: time.Since(span.Start).Milliseconds(),
			Error:      errStr,
		})
	}
}

func SpanFrom(c *gin.Context) (*Span, bool) {
	v, ok := c.Get(spanKey)
	if !ok {
		return nil, false
	}
	s, ok := v.(*Span)
	return s, ok
}

func BusFrom(c *gin.Context) (*Bus, bool) {
	v, ok := c.Get(busKey)
	if !ok {
		return nil, false
	}
	b, ok := v.(*Bus)
	return b, ok
}

// Record attaches a timed sub-operation (DB call, MQ publish, external HTTP)
// to the current request's trace as a leaf span. It emits a span.start /
// span.end pair using the caller-supplied start time so the waterfall places
// the bar at the right offset.
//
//	start := time.Now()
//	err := db.Query(...)
//	trace.Record(c, "db.users.get", start, err)
//
// Prefer trace.In / trace.StartSpan for new code — they compose with ctx
// instead of needing *gin.Context. Record remains for handlers already
// written against gin.
func Record(c *gin.Context, name string, start time.Time, err error) {
	span, ok := SpanFrom(c)
	if !ok {
		return
	}
	bus, ok := BusFrom(c)
	if !ok {
		return
	}
	spanID := newSpanID()
	var errStr string
	if err != nil {
		errStr = err.Error()
	}
	end := time.Now()
	bus.Publish(Event{
		TraceID:   span.TraceID,
		SpanID:    spanID,
		ParentID:  span.SpanID,
		Kind:      KindSpanStart,
		Name:      name,
		Service:   span.Service,
		Endpoint:  span.Endpoint,
		Timestamp: start,
	})
	bus.Publish(Event{
		TraceID:    span.TraceID,
		SpanID:     spanID,
		ParentID:   span.SpanID,
		Kind:       KindSpanEnd,
		Name:       name,
		Service:    span.Service,
		Endpoint:   span.Endpoint,
		DurationMs: end.Sub(start).Milliseconds(),
		Error:      errStr,
		Timestamp:  end,
	})
}