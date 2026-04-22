package trace

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	spanKey = "nexus.trace.span"
	busKey  = "nexus.trace.bus"
)

type spanCtxKey struct{}
type busCtxKey struct{}

// SpanFromCtx reads the current request's Span from any context.Context.
// Use this from code paths that don't have a *gin.Context — GraphQL resolvers
// (via p.Context), GORM callbacks, downstream HTTP clients.
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

type Span struct {
	TraceID  string
	Service  string
	Endpoint string
	Start    time.Time
}

var traceCounter atomic.Int64

func newTraceID() string {
	return strconv.FormatInt(traceCounter.Add(1), 36)
}

// Middleware emits request.start and request.end events bracketing the handler
// chain, and stashes a *Span and the bus in gin.Context so Record() can attach
// downstream events to the same trace.
func Middleware(bus *Bus, service, endpoint, transport string) gin.HandlerFunc {
	return func(c *gin.Context) {
		span := &Span{
			TraceID:  newTraceID(),
			Service:  service,
			Endpoint: endpoint,
			Start:    time.Now(),
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
			Kind:      KindRequestStart,
			Service:   service,
			Endpoint:  endpoint,
			Transport: transport,
			Method:    c.Request.Method,
			Path:      c.Request.URL.Path,
		})
		c.Next()
		var errStr string
		if len(c.Errors) > 0 {
			errStr = c.Errors.String()
		}
		bus.Publish(Event{
			TraceID:    span.TraceID,
			Kind:       KindRequestEnd,
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

// Record attaches a downstream event (DB call, MQ publish, external HTTP, etc.)
// to the current request's trace. Typical use:
//
//	start := time.Now()
//	err := db.Query(...)
//	trace.Record(c, "db.users.get", start, err)
func Record(c *gin.Context, name string, start time.Time, err error) {
	span, ok := SpanFrom(c)
	if !ok {
		return
	}
	bus, ok := BusFrom(c)
	if !ok {
		return
	}
	var errStr string
	if err != nil {
		errStr = err.Error()
	}
	bus.Publish(Event{
		TraceID:    span.TraceID,
		Kind:       KindDownstream,
		Service:    span.Service,
		Endpoint:   span.Endpoint,
		Message:    name,
		DurationMs: time.Since(start).Milliseconds(),
		Error:      errStr,
	})
}